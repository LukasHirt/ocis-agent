# ocis-agent

An always-on personal agent for oCIS: a Go sidecar (`backend/`) plus an oCIS Web
extension (`frontend/`).

## Layout

- `backend/` Go module `github.com/owncloud/ocis-agent/backend`
- `frontend/` `web-app-agent`, the oCIS Web extension (Vue 3 + TypeScript)
- `docker-compose.yml` + `dev/` dev stack (oCIS + TLS proxy), started from the repo root
- `Dockerfile` + `deploy/docker-compose.example.yml` container image of the sidecar and an example
  deployment next to an existing oCIS

## Documentation

- [docs/deployment.md](docs/deployment.md) deployment, configuration (every `AGENT_*` variable),
  operations and security checklist
- [docs/api.md](docs/api.md) the REST API under `/agent-api/`

## Container image

    docker build -t ocis-agent:dev .

The image holds the static binary only and runs as uid 10001 with its database in the `/data`
volume, so the root filesystem can be read-only. All settings come from the environment;
`AGENT_MASTER_KEY` is environment-only, so pass it with an env_file of mode 0600 (never bake it into
an image). Port 9300 serves `/agent-api/` (health check: `GET /agent-api/healthz`), port 9301 only
`/healthz` and `/readyz` for probes. Give the container a stop grace period of at least
`AGENT_SHUTDOWN_DRAIN` + 15s (45s with the defaults). See `deploy/docker-compose.example.yml`.

## Contributing

See `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md` and `SUPPORT.md`.
Commits use Conventional Commits with a DCO sign-off (`git commit -s`).
Licensed under Apache-2.0 (`LICENSE`, `REUSE.toml`); run `make license-check`.

## Prerequisites

Go 1.26, Node >= 24.15, pnpm 10.33.3 (via corepack), Docker with compose, openssl, and uv or pipx for `reuse`.

## Dev quick start

    make dev-up         # builds the extension, creates .env and a self-signed cert if
                        # missing, starts oCIS + nginx on https://localhost:9200
                        # (port is OCIS_DEV_PORT in .env; published on 127.0.0.1 only)
    make sidecar        # sidecar on :9300, routed at /agent-api/; its dev master key and
                        # database persist in backend/bin/data (delete it to reset; never use the key elsewhere)

Log in at https://localhost:9200 (port is OCIS_DEV_PORT in .env; changing it after the first start needs `docker compose down -v`) as `admin` / `admin` or a demo user such as `einstein` / `relativity`.

The built extension is served by oCIS at `/assets/apps/agent/manifest.json`.

The sidecar binds `:9300` by default. On macOS/Docker Desktop `AGENT_HTTP_ADDR=127.0.0.1:9300` also works. On Linux the nginx container reaches the host through the docker bridge (host-gateway), so a loopback-only sidecar is NOT reachable from nginx there; keep the default.
