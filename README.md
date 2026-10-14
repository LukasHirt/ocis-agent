# ocis-agent

An always-on personal agent for oCIS: a Go sidecar (`backend/`) plus an oCIS Web
extension (`frontend/`).

## Layout

- `backend/` Go module `github.com/owncloud/ocis-agent/backend`
- `frontend/` `web-app-agent`, the oCIS Web extension (Vue 3 + TypeScript)
- `docker-compose.yml` + `dev/` dev stack (oCIS + TLS proxy), started from the repo root

## Contributing

See `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md` and `SUPPORT.md`.
Commits use Conventional Commits with a DCO sign-off (`git commit -s`).
Licensed under Apache-2.0 (`LICENSE`, `REUSE.toml`); run `make license-check`.

## Dev quick start

    make dev-up         # builds the extension, creates .env and a self-signed cert if
                        # missing, starts oCIS + nginx on https://localhost:9200
    AGENT_OCIS_URL=https://localhost:9200 \
    AGENT_MASTER_KEY=$(openssl rand -base64 32) \
    AGENT_INSECURE=true \
    go run -C backend ./cmd/ocis-agent    # sidecar on :9300, routed at /agent-api/

Log in at https://localhost:9200 as `admin` / `admin` or a demo user such as `alice` / `demo`.
