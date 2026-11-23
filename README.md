# oCIS Agent Docker Image

[![Docker Pulls](https://img.shields.io/docker/pulls/owncloud/ocis-agent.svg)](https://hub.docker.com/r/owncloud/ocis-agent)
[![License: Apache-2.0](https://img.shields.io/github/license/owncloud-docker/ocis-agent)](https://github.com/owncloud-docker/ocis-agent/blob/main/LICENSE)
[![ownCloud OSPO](https://img.shields.io/badge/OSPO-ownCloud-blue)](https://kiteworks.com/opensource)

Docker image for the [oCIS Agent](https://github.com/owncloud/ocis-agent) sidecar — an
always-on personal agent for [ownCloud Infinite Scale (oCIS)](https://github.com/owncloud/ocis).
It runs next to oCIS and serves the REST API that the agent's ownCloud Web extension talks
to. This repo builds and publishes the image; the application source lives in
[owncloud/ocis-agent](https://github.com/owncloud/ocis-agent).

> **Build & maintenance:** see [how these images are built, scanned, updated and published](https://github.com/owncloud-docker/.github/blob/master/docs/IMAGE-LIFECYCLE.md).

## Quick Start

```bash
umask 077
printf 'AGENT_MASTER_KEY=%s\n' "$(openssl rand -base64 32)" > agent.env

docker run -d --name ocis-agent \
  --env-file agent.env \
  -e AGENT_OCIS_URL=https://ocis.example.com \
  -e AGENT_LLM_BASE_URL=https://api.openai.com/v1 \
  -e AGENT_LLM_MODEL=gpt-4.1-mini \
  -e AGENT_LLM_API_KEY_FILE=/run/secrets/llm_api_key \
  -v "$PWD/llm_api_key:/run/secrets/llm_api_key:ro" \
  -v ocis-agent-data:/data \
  -p 127.0.0.1:9300:9300 \
  --read-only --tmpfs /tmp \
  --stop-timeout 45 \
  owncloud/ocis-agent:latest
```

- `AGENT_MASTER_KEY` (base64, 32 bytes) and `AGENT_OCIS_URL` (the public origin of oCIS)
  are required. The key is read from the environment only: keep `agent.env` at mode
  `0600`, back it up separately from the database, and never bake it into an image.
- The default roles (`AGENT_ROLES=api,worker`) need a model endpoint
  (`AGENT_LLM_BASE_URL`, `AGENT_LLM_MODEL`, `AGENT_LLM_API_KEY` or
  `AGENT_LLM_API_KEY_FILE`). Run exactly one process with the `worker` role per database.
- Route `https://<oCIS host>/agent-api/` from the reverse proxy in front of oCIS to port
  `9300`, with response buffering off and a long read timeout (server-sent events). Never
  route `/agent/`: those are the web extension's UI routes and must reach oCIS Web.
- oCIS needs app tokens: `OCIS_ADD_RUN_SERVICES=auth-app` and `PROXY_ENABLE_APP_AUTH=true`.
- The web extension is not part of this image; install it into oCIS Web as described
  upstream.

See the upstream
[deployment guide](https://github.com/owncloud/ocis-agent/blob/main/docs/deployment.md)
for every `AGENT_*` setting, and
[`deploy/docker-compose.example.yml`](https://github.com/owncloud/ocis-agent/blob/main/deploy/docker-compose.example.yml)
for a hardened Compose example.

## Supported Tags

| Tag | Meaning |
|-----|---------|
| `latest` | The highest released version |
| `X.Y.Z` | A specific release (e.g. `0.1.0`), rebuilt in place while it is the newest one |
| `X.Y.Z-YYYYMMDD` | The build of a release on a given day (e.g. `0.1.0-20261123`); never touched again after that day |

> **Note:** oCIS Agent is pre-1.0. Only plain `vX.Y.Z` upstream release tags are built;
> there are no floating `X.Y` or `X` tags. For production, pin an `X.Y.Z` or
> `X.Y.Z-YYYYMMDD` tag instead of `latest`. A second rebuild on the same day replaces that
> day's `X.Y.Z-YYYYMMDD` tag; pin by digest if you need the exact bytes.

```bash
docker pull owncloud/ocis-agent:latest
```

## How New Releases Get Built

- **New releases:** CI checks the upstream repository for new release tags every 6 hours
  and builds every release that is not on Docker Hub yet. Maintainers can start the same
  check by hand (`workflow_dispatch`) instead of waiting.
- **Signed tags only:** a release is built only from an annotated upstream tag whose
  signature GitHub verifies. Other tags are skipped with a warning.
- **Security rebuilds:** the newest release is rebuilt and re-pushed whenever a change is
  merged into this repository, including base-image and Go toolchain updates, and once a
  week, picking up Alpine package fixes.
- **Scanning:** every build is scanned with Trivy and smoke-tested before it is
  published. An image with a fixable HIGH or CRITICAL vulnerability is never pushed.

## Volumes

| Path | Purpose |
|------|---------|
| `/data` | All sidecar state (`AGENT_DATA_DIR`): the SQLite database. The app tokens in it are sealed with `AGENT_MASTER_KEY`, so a backup is useless without the same key |

Everything else can be read-only (`--read-only`). Take consistent backups while the
sidecar runs with `docker exec ocis-agent ocis-agent backup /data/agent-backup.db`, then
copy the file out of the volume.

## Ports

| Port | Protocol | Purpose |
|------|----------|---------|
| `9300` | TCP | The REST API under `/agent-api/` (`AGENT_HTTP_ADDR`); health check `GET /agent-api/healthz` |
| `9301` | TCP | `GET /healthz` and `/readyz` for probes only (`AGENT_DEBUG_ADDR`) — never publish it |

## User and Filesystem

The image is based on Alpine Linux and runs as uid/gid `10001`. `/data` is owned by that
user with mode `0700`. A fresh named volume takes that owner automatically; when you
bind-mount a host directory instead, `chown 10001:10001` it and `chmod 0700` it.

The image's `HEALTHCHECK` polls `http://127.0.0.1:9300/agent-api/healthz`. A worker-only
process (`AGENT_ROLES=worker`) serves no API: override the check with
`http://127.0.0.1:9301/readyz`.

On `SIGTERM` the sidecar drains running jobs for up to `AGENT_SHUTDOWN_DRAIN` (default
`30s`) and then requeues them, so give the container a stop grace period of at least
`AGENT_SHUTDOWN_DRAIN` + 15s.

## Build Arguments

| ARG | Default | Purpose |
|-----|---------|---------|
| `GIT_REF` | `main` | `ocis-agent` git ref (tag or branch) to clone and build, e.g. `v0.1.0` |
| `GIT_SHA` | `""` | Optional exact commit to check out after cloning `GIT_REF`. CI passes the commit the verified tag points to |
| `VERSION` | `""` | Version string embedded in the OCI labels, e.g. `0.1.0` |
| `REVISION` | `""` | Git commit embedded in the OCI labels |
| `TARGETARCH` | set by buildx | Target architecture (`amd64`, `arm64`) |

## Building

The image is built entirely from source via a three-stage `Dockerfile.multiarch`:

**`source`** — clones `ocis-agent` at `${GIT_REF}`, pinned to `${GIT_SHA}` when set.

**`build`** — compiles the static Go binary (`backend/cmd/ocis-agent`) with
`CGO_ENABLED=0` for the target architecture.

**Runtime** — Alpine with the binary, the licence files and the `/data` directory.

```bash
docker buildx build -f Dockerfile.multiarch \
  --build-arg GIT_REF=v0.1.0 \
  --build-arg VERSION=0.1.0 \
  -t ocis-agent:test .
```

## CI

The GitHub Actions workflow (`.github/workflows/main.yml`) runs on pull requests,
pushes to `main`, a schedule and by hand:

1. **Test** — lints the scripts and tests the release resolution logic.
2. **Resolve** — works out which releases to build (see
  [How New Releases Get Built](#how-new-releases-get-built)). A pull request always builds
  the newest release (or upstream `main` while there is none, or it does not verify), without publishing it.
3. **Build** — multi-arch image (`linux/amd64`, `linux/arm64`), built natively on each
  architecture.
4. **Trivy scan** — fails on HIGH/CRITICAL CVEs that have a fix; accepted exceptions are
  listed with a justification in `.trivyignore`.
5. **Smoke test** — starts the container with the `api` role and polls
  `http://localhost:9300/agent-api/healthz`.
6. **Publish** — pushes to Docker Hub and syncs this README as the image description
  (not on pull requests).

## Community & Support

- [ownCloud Website](https://owncloud.com)
- [Community Discussions](https://github.com/orgs/owncloud/discussions)
- [Matrix Chat](https://app.element.io/#/room/#owncloud:matrix.org)
- [Documentation](https://doc.owncloud.com)
- [Enterprise Support](https://owncloud.com/contact-us/)
- [OSPO Home](https://kiteworks.com/opensource)

See [SUPPORT.md](SUPPORT.md) for the full list of support channels.

## Contributing

We welcome contributions! Please read the [Contributing Guidelines](CONTRIBUTING.md)
and our [Code of Conduct](CODE_OF_CONDUCT.md) before getting started.

- **Rebase Early, Rebase Often!** We use a rebase workflow — rebase on the target
  branch before submitting a PR.
- **Signed commits**: All commits **must** be PGP/GPG signed and carry a DCO
  `Signed-off-by` line (`git commit -S -s`).
- **Conventional Commits**: PR titles must follow the
  [Conventional Commits](https://www.conventionalcommits.org/) format — enforced
  by CI.
- **GitHub Actions Policy**: Workflows may only use actions owned by `owncloud`,
  created by GitHub (`actions/*`), or verified in the GitHub Marketplace, pinned
  to a full commit SHA.

Note: bug reports and feature requests for oCIS Agent itself belong upstream in
[`owncloud/ocis-agent`](https://github.com/owncloud/ocis-agent) — this repository
tracks only the Docker packaging.

## Security

**Do not open a public GitHub issue for security vulnerabilities.**

Report vulnerabilities at **<https://security.owncloud.com>** — see [SECURITY.md](SECURITY.md).

## About the ownCloud OSPO

The [Kiteworks Open Source Program Office](https://kiteworks.com/opensource), operating under
the [ownCloud](https://owncloud.com) brand, launched on May 5, 2026, to steward the open source
ecosystem around ownCloud's products. The OSPO ensures transparent governance, license compliance,
community health, and sustainable collaboration between the open source community and
[Kiteworks](https://www.kiteworks.com), which acquired ownCloud in 2023.

- **OSPO Home**: <https://kiteworks.com/opensource>
- **GitHub**: <https://github.com/owncloud>
- **ownCloud**: <https://owncloud.com>

For questions about the OSPO or licensing, contact ospo@kiteworks.com.

This repository is licensed under the **Apache License 2.0**, which is the license
the OSPO is adopting across the ecosystem. No relicensing is required.

## License

Apache-2.0 — see [LICENSE](LICENSE).
