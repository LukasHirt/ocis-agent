# Deploying ocis-agent

This guide covers running the ocis-agent sidecar next to an existing ownCloud Infinite Scale
(oCIS) installation: architecture, prerequisites, configuration, operations and security. The REST
API is described in [api.md](api.md).

## Architecture

```
browser ── https ──> reverse proxy (TLS) ──┬── /agent-api/  ──> ocis-agent sidecar :9300
                                           └── everything else ──> oCIS (incl. /agent/ UI routes)
ocis-agent ── https ──> oCIS public APIs (Graph, WebDAV, auth-app, notifications SSE)
ocis-agent ── https ──> model endpoint (OpenAI-compatible chat completions)
```

- **oCIS** serves oCIS Web and the agent's web extension (`web-app-agent`), which adds the agent's
  screens under `/agent/` in oCIS Web.
- **The sidecar** (`ocis-agent`, one static Go binary) serves the REST API under `/agent-api/`,
  runs the jobs and keeps its state in one SQLite file. It talks to oCIS only through public HTTP
  APIs. To act for a user it holds an oCIS app token per user, created at enrolment with the
  user's own login, sealed with the master key and renewed in the background.
- **The reverse proxy** puts both on one origin, so the extension calls `/agent-api/` on the oCIS
  host and the sidecar's Origin check works.

## Prerequisites

### oCIS

- App tokens must be enabled: run the auth-app service and accept app tokens at the proxy:

  ```
  OCIS_ADD_RUN_SERVICES=auth-app
  PROXY_ENABLE_APP_AUTH=true
  ```

  Keep `AUTH_APP_ENABLE_IMPERSONATION=false`; the sidecar never needs it. Without app auth,
  enrolment fails or the agent's calls get `401` and the agent pauses itself with
  `credential_invalid`.
- The agent calls oCIS with its app token by Basic auth, whose user name is the user's login name
  (Graph `onPremisesSamAccountName`); accounts without one cannot enrol (`no_login_name`).

### The web extension

Build the extension (`make frontend-build`, output in `frontend/dist`) and install it into the oCIS
web assets as the app `agent`, so that `/assets/apps/agent/manifest.json` is served:

- copy or mount `frontend/dist` to `agent/` in the oCIS web apps directory (`WEB_ASSET_APPS_PATH`,
  by default `$OCIS_BASE_DATA_PATH/web/assets/apps`, which is `/var/lib/ocis/web/assets/apps` in
  the oCIS container; the dev stack mounts it read-only at `/var/lib/ocis/web/assets/apps/agent`);
- restart oCIS if the app does not show up.

### Reverse proxy

Route `/agent-api/` to the sidecar and everything else to oCIS. Requirements:

- **Never route `/agent/`** (without `-api`) to the sidecar: those are the extension's UI routes
  and must reach oCIS Web. Match the `/agent-api/` prefix exactly.
- Response buffering off and a long read timeout for `/agent-api/v1/events` (a server-sent event
  stream lasting up to about 65 minutes, with a ping every 25 s). The sidecar sends
  `X-Accel-Buffering: no` on the stream, but set it in the proxy too.
- Pass the `Host`, `Origin` and `Authorization` headers unchanged. The sidecar does not trust
  `X-Forwarded-For`: rate limit per real client at the proxy.
- Terminate TLS at the proxy; keep the sidecar's port on loopback or a private network.

nginx:

```nginx
location /agent-api/ {
    proxy_pass http://127.0.0.1:9300;
    proxy_http_version 1.1;
    proxy_buffering off;
    proxy_read_timeout 1h;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto https;
}

location / {
    proxy_pass http://ocis:9200;   # your oCIS upstream
    # ...
}
```

Traefik (labels on the sidecar container; the oCIS router keeps `PathPrefix(`/`)`, which has a
lower priority than the longer rule):

```yaml
labels:
  - traefik.http.routers.ocis-agent.rule=Host(`ocis.example.com`) && PathPrefix(`/agent-api/`)
  - traefik.http.routers.ocis-agent.entrypoints=websecure
  - traefik.http.routers.ocis-agent.tls=true
  - traefik.http.services.ocis-agent.loadbalancer.server.port=9300
  # Traefik streams responses without buffering by default; give the entrypoint a long
  # read/idle timeout (entryPoints.websecure.transport.respondingTimeouts) for the event stream
```

### A model endpoint

The worker role needs an OpenAI-compatible chat-completions endpoint with tool calling (see
[Model setup](#model-setup)). An api-only process does not.

## Running the sidecar

### Container image

```
docker build -t ocis-agent:dev .
```

The image is a small Alpine base (busybox `wget` serves the health check) with the static binary;
it holds no configuration or secrets and runs as uid/gid 10001 with its database in the `/data`
volume, so the root filesystem can be read-only. Port 9300 serves `/agent-api/`, port 9301 only the
probes (`/healthz`, `/readyz`; never publish it). `deploy/docker-compose.example.yml` is a complete
example next to an existing oCIS: env_file for the master key, a docker secret for the model key,
`read_only`, `cap_drop: ALL`, `no-new-privileges`, health checks and the stop grace period.

- **File ownership on Linux.** Docker keeps the host owner of bind mounts and file-based secrets.
  A bind-mounted data directory must be owned by `10001:10001` (mode 0700), and a secret file
  read through `AGENT_LLM_API_KEY_FILE` must be readable by uid 10001 (`chown 10001` it, keep mode
  0600). A fresh named volume takes the owner of the image's `/data` automatically. Docker Desktop
  (macOS, Windows) maps ownership, so this only shows up on Linux.
- **Health checks.** The image's `HEALTHCHECK` calls `http://127.0.0.1:9300/agent-api/healthz`. If
  you change `AGENT_HTTP_ADDR` or `AGENT_DEBUG_ADDR`, override the health check too. A worker-only
  container (`AGENT_ROLES=worker`) has no API listener: check `http://127.0.0.1:9301/readyz`
  instead.

### Binary

```
ocis-agent                          # run the roles named by AGENT_ROLES
ocis-agent migrate                  # apply the database migrations and exit
ocis-agent killswitch on|off|status # set or show the kill switch in the database
ocis-agent backup <dest-file>       # write a verified snapshot of the database
```

Every setting comes from the environment. Logs are JSON lines on stderr.

### Roles and the single-worker rule

`AGENT_ROLES` selects what a process runs:

- `api`: the REST API on `AGENT_HTTP_ADDR`. Needs no model; with no model configured the model
  catalog is empty.
- `worker`: the job workers (`AGENT_WORKERS`) and the scheduler, which does all maintenance: token
  renewal and the token sweep, approval expiry, the dormancy pause, the lease reaper and the
  settling of approvals of ended jobs. It needs the `AGENT_LLM_*` settings; start-up refuses
  otherwise.

**Run exactly one process with the worker role per database.** An api-only process runs no
maintenance (it logs that once at start-up) and its chat jobs stay queued until a worker process
runs. Several api-only processes on the same database are possible in principle, but the SQLite
database is a local file, so in practice run one process with the default `api,worker`.

## Configuration

All variables are optional unless marked required. Durations use Go syntax (`90s`, `15m`, `72h`).

| Variable | Default | Meaning |
| --- | --- | --- |
| `AGENT_OCIS_URL` | required | The public origin of oCIS, `scheme://host[:port]` without a path (for example `https://ocis.example.com`). The sidecar calls oCIS here. |
| `AGENT_ALLOWED_ORIGIN` | `AGENT_OCIS_URL` | The browser origin state-changing requests must come from (normalised: lower-case host, no default port). |
| `AGENT_MASTER_KEY` | required | Base64 of exactly 32 random bytes. Seals the app tokens in the database. Environment only (there is no file variant); see [Master key](#master-key). |
| `AGENT_HTTP_ADDR` | `:9300` | The API listener. |
| `AGENT_DEBUG_ADDR` | `:9301` | The probe listener (`GET /healthz`, `GET /readyz` only, no authentication); `off` disables it. |
| `AGENT_DATA_DIR` | `/var/lib/ocis-agent` | Directory of the database. The image sets `/data`. At start-up the directory that holds the database file (this one, or that of `AGENT_SQLITE_PATH`) is created with mode 0700 if missing. |
| `AGENT_SQLITE_PATH` | `<AGENT_DATA_DIR>/agent.db` | The database file, if it should live elsewhere. |
| `AGENT_INSECURE` | `false` | Skip TLS certificate verification towards oCIS. Development with self-signed certificates only. |
| `AGENT_ROLES` | `api,worker` | Comma-separated subset of `api` and `worker`. |
| `AGENT_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `AGENT_SHUTDOWN_DRAIN` | `30s` | How long a shutdown waits for running jobs before it requeues them (0s to 10m). |
| `AGENT_KILL_SWITCH` | `false` | `true` holds the kill switch on regardless of the database flag: no job is claimed. |
| `AGENT_WORKERS` | `4` | Concurrent job workers (1 to 64). |
| `AGENT_MAX_STEPS` | `30` | Model turns per job (1 to 200). |
| `AGENT_MAX_ATTEMPTS` | `3` | Crashed runs a job may have (1 to 20). |
| `AGENT_MAX_RUNS` | `20` | Claims per job, the cap that ends requeue loops (1 to 1000). |
| `AGENT_LEASE_TTL` | `2m` | Job lease; a worker renews it every third (1s to 1h). |
| `AGENT_APPROVAL_TTL` | `168h` | How long an approval stays pending before it expires (1m to 8760h). |
| `AGENT_TOKEN_TTL` | `72h` | Lifetime of the agent's app tokens; they are renewed below half of it (1h to 720h). |
| `AGENT_TOKEN_SWEEP_INTERVAL` | `15m` | How often the token sweep checks each user's app tokens (1m to 24h). |
| `AGENT_DORMANCY_DAYS` | `30` | Days without a visit before the agent pauses itself (`dormant`) for a user; 0 disables it (0 to 3650). |
| `AGENT_API_RATE` | `20` | Requests per second per user (0.5 to 1000); the per-IP failed-login bucket gets 10 times this. |
| `AGENT_API_BURST` | `40` | Burst of the per-user rate limit (1 to 5000). |
| `AGENT_API_STREAMS_PER_USER` | `3` | Open event streams per user (1 to 10). |
| `AGENT_API_STREAMS_TOTAL` | `200` | Open event streams in the process (1 to 5000). |
| `AGENT_USER_DAILY_TOKENS` | `200000` | The operator's cap and default of a user's daily model-token budget (1000 to 1000000000). |
| `AGENT_LLM_BASE_URL` | none | The model endpoint, for example `https://api.openai.com/v1`. https, or http only for a loopback host. Required by the worker role and by the other `AGENT_LLM_*` variables of this table except `AGENT_LLM_MAX_TOKENS`. |
| `AGENT_LLM_PROVIDER` | `openai` | The wire protocol; only `openai` (OpenAI-compatible chat completions). |
| `AGENT_LLM_MODEL` | none | The default model id; required with `AGENT_LLM_BASE_URL`. |
| `AGENT_LLM_MODELS` | none | More models users may choose: comma-separated `id` or `id=Label`. |
| `AGENT_LLM_API_KEY` | none | The model key. Required unless the host is loopback. Printable ASCII without spaces. |
| `AGENT_LLM_API_KEY_FILE` | none | A file holding the model key (for docker or Kubernetes secrets); exclusive with `AGENT_LLM_API_KEY`. |
| `AGENT_LLM_ALLOWED_HOSTS` | the base URL's host | Comma-separated egress allow-list for the model client; must contain the base URL's host. |
| `AGENT_LLM_TIMEOUT` | `120s` | Bound of one model call, reading the response included (10s to 30m). |
| `AGENT_LLM_REASONING_EFFORT` | not sent | `none`, `low`, `medium` or `high`, sent as `reasoning_effort`. |
| `AGENT_LLM_MAX_TOKENS` | `4096` | Output token limit per model call (64 to 100000). |

A value outside its range stops the start with an error that names the variable. Errors never
contain the master key or the model key; an unreadable `AGENT_LLM_API_KEY_FILE` is reported with the
operating system's error, which includes the file's path.

## Model setup

The sidecar speaks the OpenAI chat-completions protocol with tool calling. Any compatible endpoint
works if the model handles tools reliably. The client talks only to the configured base URL (and
the allowed hosts), never follows redirects and never logs the key.

Hosted provider:

```
AGENT_LLM_BASE_URL=https://api.openai.com/v1
AGENT_LLM_MODEL=gpt-4.1-mini
AGENT_LLM_MODELS=gpt-4.1=GPT-4.1
AGENT_LLM_API_KEY_FILE=/run/secrets/llm_api_key
```

Local [Ollama](https://ollama.com) on the same host as a sidecar binary:

```
AGENT_LLM_BASE_URL=http://localhost:11434/v1
AGENT_LLM_MODEL=qwen3:14b
AGENT_LLM_TIMEOUT=10m
```

Plain http and no key are allowed only because the host is loopback. Notes:

- **Inside a container, the Docker host is not loopback.** `http://host.docker.internal:11434` is
  refused (https required). The options:
  - `network_mode: host` (Linux only; Docker Desktop on macOS and Windows does not share the
    host's network this way): `localhost` is the host, and the sidecar's ports are the host's.
  - `network_mode: service:ollama`, with Ollama in a container: the sidecar joins Ollama's network
    namespace, so port 9300 must be published on (or reached through) the `ollama` service, not
    the sidecar's. On a Mac, Ollama in a container has no GPU (Metal) acceleration.
  - Run the sidecar binary on the host instead of in a container (`make sidecar` in development),
    next to an Ollama on the host. This is the simplest choice on macOS.
  - Development only: a loopback forwarder in the sidecar's network namespace, for example a
    `socat` container with `network_mode: service:ocis-agent` running
    `socat TCP-LISTEN:11434,bind=127.0.0.1,fork,reuseaddr TCP:host.docker.internal:11434`. The
    sidecar then uses `http://127.0.0.1:11434/v1`, which is loopback for the configuration, but the
    traffic crosses the Docker bridge in plain text. On Linux, add
    `extra_hosts: ["host.docker.internal:host-gateway"]` to the sidecar service.
  - Put Ollama behind a TLS proxy and use its https URL. A non-loopback endpoint also needs an
    `AGENT_LLM_API_KEY`, which Ollama ignores, so any placeholder value works. The model client
    trusts only the system CA bundle (`AGENT_INSECURE` does not apply to it): for a self-signed or
    private-CA certificate mount the CA and set `SSL_CERT_FILE=/path/to/ca.pem` (or `SSL_CERT_DIR`).
    Without that, every call fails at TLS, counts as an outage and the jobs end as "restarted too
    many times", exactly like a timeout.
- **Timeouts.** Local models are slow; a call that exceeds `AGENT_LLM_TIMEOUT` counts as an outage
  and the job is requeued with a delay, until `AGENT_MAX_RUNS` ends it ("restarted too many
  times"). Raise the timeout to a few minutes for local models.
- **Context length.** The system prompt with the memory excerpt and the tool definitions is
  several thousand tokens, and every tool result adds to it. Ollama's default context window is
  small and it silently drops the start of a longer prompt, which shows up as the model ignoring
  its instructions or tools. Raise it on the Ollama server (`OLLAMA_CONTEXT_LENGTH=32768`, or
  `num_ctx` in a Modelfile); the sidecar has no context-length setting.
- **Reasoning effort.** `AGENT_LLM_REASONING_EFFORT` is sent only when set. Reasoning models get
  faster with `low` or `none`; leave it unset for endpoints that reject the parameter.
- **Answers cut off.** A response that hits `AGENT_LLM_MAX_TOKENS` is never acted on; the job
  fails with "cut off before it was complete". Raise the limit if that happens often.

## Data, backup and restore

All state is in the SQLite database (`AGENT_SQLITE_PATH`, by default `agent.db` in the data
directory) plus its `-wal` and `-shm` files: users, sealed app tokens, policies, jobs, approvals,
conversations and the hash-chained audit log. The agent's memory files live in each user's oCIS
personal space under `/.ocis-agent/` and are backed up with oCIS.

Backup, also while the sidecar runs:

```
ocis-agent backup /backups/agent-2026-10-21.db
# in the container:
docker compose exec ocis-agent ocis-agent backup /data/agent-backup.db
```

It writes one consistent snapshot (mode 0600; it refuses an existing file), checks the source and
the snapshot (integrity check and schema version) and needs only `AGENT_DATA_DIR` or
`AGENT_SQLITE_PATH`, not the master key. The snapshot holds the app tokens sealed; it is useless
without the master key, so **back up the master key separately** and keep the two apart.

Restore:

1. Stop the sidecar.
2. Copy the snapshot to `<data dir>/agent.db` and delete `agent.db-wal` and `agent.db-shm` if
   present (in the container, owned by uid 10001, mode 0600).
3. Start the sidecar with the same `AGENT_MASTER_KEY`. A different key stops the start-up with an
   error naming `AGENT_MASTER_KEY`.

The audit chain verifies after a restore. Token renewals after the snapshot are not in it: the
restored app token may have been deleted by a later renewal (the agent then pauses with a
credential reason and the user enrols again), and a token created after the snapshot has a label
the restored database does not know, so the sweep pauses the agent with `token_anomaly` until the
user checks and resumes.

## Master key

- Generate it once: `openssl rand -base64 32`. The sidecar refuses to start without it.
- Pass it only through the environment, for example an env_file of mode 0600 outside version
  control. Never bake it into an image, a compose file or a repository; never log or print it.
- A lost key cannot be recovered: every user has to enrol again with a fresh database.
- **There is no master-key rotation yet.** A changed key stops the start. If the key may have
  leaked, treat the sealed app tokens as compromised: users run the incident revoke (revoke all
  app tokens) or an oCIS admin deletes them, then start with a new key and a new database (keep a
  backup of the old one for its audit log). An internal rotation of the data-encryption keys
  exists, but no command exposes it, and it would not replace the master key anyway.

## Shutdown

On SIGINT or SIGTERM the process stops claiming jobs and the scheduler, `/readyz` turns `503`, the
event streams end with `bye`, the API listener stops gracefully (up to 10 s), running jobs get up to
`AGENT_SHUTDOWN_DRAIN` (counted from the signal) to finish and are then cancelled and requeued. A
second signal ends the process at once.

**Give the container a grace period of at least `AGENT_SHUTDOWN_DRAIN` + 15s** (45 s with the
defaults; the Kubernetes default of 30 s is too short): `stop_grace_period: 45s` in compose,
`terminationGracePeriodSeconds: 45` in Kubernetes, `docker stop -t 45`. A process killed earlier
leaves its jobs to the lease reaper, which recovers them only after the lease expires.

## Kill switch

Stops all agent work without stopping the process: no job is claimed and a running job stops at its
next step. Maintenance (token renewal, expiry, dormancy) keeps running and the API keeps answering.

```
ocis-agent killswitch on      # needs the same AGENT_DATA_DIR (or AGENT_SQLITE_PATH), a well-formed AGENT_MASTER_KEY
ocis-agent killswitch status  # and AGENT_OCIS_URL, since the configuration is loaded first
ocis-agent killswitch off
```

Running workers notice the database flag without a restart. `AGENT_KILL_SWITCH=true` holds it on
regardless of the flag. The command refuses a missing database instead of creating one. Users see
both flags in the status (`kill_switch`).

## Upgrades and migrations

The database schema is migrated at start-up; `ocis-agent migrate` applies the migrations and
exits (useful as an init step). To upgrade: take a backup, stop the old process, start the new
one. Migrations only go forward: an older binary refuses a database with a newer schema ("newer
than this binary supports"), so a downgrade means restoring the backup taken before the upgrade.

## Security checklist

- TLS everywhere: browser to proxy, sidecar to oCIS (`AGENT_OCIS_URL` with https;
  `AGENT_INSECURE` only for development) and sidecar to the model (https unless loopback).
- Bind the sidecar's ports to loopback or a private network; never publish the debug port 9301.
- `AGENT_ALLOWED_ORIGIN` must be the oCIS origin users load (it defaults to `AGENT_OCIS_URL`); serve
  `/agent-api/` on that same origin, never cross-origin.
- Rate limit per client at the reverse proxy; the sidecar's per-IP bucket sees only the proxy.
- Keep `AGENT_TOKEN_TTL` short (default 72 h). oCIS lets an app token create new app tokens, so a
  stolen copy can renew itself and the TTL alone does not bound it: the sweep
  (`AGENT_TOKEN_SWEEP_INTERVAL`) pauses the agent (`token_anomaly`) on unknown tokens with the
  agent's label prefix, and the incident revoke deletes all of a user's app tokens.
- Keep dormancy on (`AGENT_DORMANCY_DAYS`): users who stop visiting get a paused agent.
- Keep `AGENT_USER_DAILY_TOKENS` at a level you are willing to pay per user and day.
- Keep the master key and the database backups apart, both readable only by the service user.
- Every destructive or outward action needs the user's approval, enforced by the sidecar from its
  database; no setting turns that off.
- The sidecar never logs tokens, keys or error texts from oCIS or the model; log lines carry ids,
  counts and fixed categories.

## Monitoring

- **Probes.** `GET /agent-api/healthz` on the API port: liveness of the API. `GET /healthz` and
  `GET /readyz` on the debug port: `/readyz` is `503` while shutting down, before the secrets are
  ready, when the database does not answer, or (worker role) without a live worker. Use `/readyz`
  for readiness and the API's healthz for liveness.
- **Logs.** JSON lines on stderr. Every API request logs one line (`request_id`, method, route
  pattern, status, duration, user id, error category); the request id is in the `X-Request-Id`
  header and every error body, so a user's report can be matched. Health-check requests are logged
  too. Useful categories: `rate_limited`, `upstream_unavailable` (oCIS failures),
  `unauthenticated`, `panic`, `poll_failed`; worker and scheduler lines report job ids and fixed
  outcomes.
- **Inside the extension.** Users see pauses (`pause_reason`), the kill switch and their budget in
  the status banner, and every action in the activity (audit) log. There is no other notification
  channel.

## Troubleshooting

- **`bind: address already in use` at start-up.** Another process holds `:9300` or the debug port
  `:9301`, often a second sidecar (a development one, or another test run). Stop it, or set
  `AGENT_HTTP_ADDR` / `AGENT_DEBUG_ADDR` to free ports (`AGENT_DEBUG_ADDR=off` disables the probe
  listener; adjust the container health check).
- **Chat answers never come; jobs end with "restarted too many times".** The model calls time out
  or fail (outage, rate limit) and the job is requeued until `AGENT_MAX_RUNS`. Check the model
  endpoint and raise `AGENT_LLM_TIMEOUT` for slow local models. Jobs that stay `queued` mean no
  worker runs: check `AGENT_ROLES`, the kill switch and the user's pause.
- **"The language model provider refused the configured credentials."** The model key is wrong or
  missing (`AGENT_LLM_API_KEY` / `AGENT_LLM_API_KEY_FILE`).
- **The extension keeps asking the user to log in, or every request is `401`.** The sidecar rejects
  bearers that oCIS does not accept for Graph `/me`. Check that the sidecar reaches oCIS at
  `AGENT_OCIS_URL` (TLS errors with a private CA appear as `upstream_unavailable`, not `401`: mount
  the CA and set `SSL_CERT_FILE` or `SSL_CERT_DIR`, or `AGENT_INSECURE=true` in development only), that
  `AGENT_OCIS_URL` is the same oCIS the browser is logged in to, and the proxy passes the
  `Authorization` header. A `403 origin_rejected` on every change means `AGENT_ALLOWED_ORIGIN` does
  not match the origin in the browser's address bar.
- **The agent pauses itself with `credential_invalid`** after enrolment. oCIS refuses the app
  token: check `PROXY_ENABLE_APP_AUTH=true` and the auth-app service, then enrol again.
- **`token_anomaly` pause.** The sweep found an app token with the agent's label that the sidecar
  did not create (for example a restored older database, or a copied token). Check the user's app
  tokens in oCIS, run the incident revoke if in doubt, then resume.
- **A memory file right after a save answers `503` with `Retry-After`.** oCIS is still processing
  the fresh upload; the client retries.
- **Start-up fails naming `AGENT_MASTER_KEY`.** The key is missing, not base64 of 32 bytes, or not
  the key the database was created with.
