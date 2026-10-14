# AGENTS.md

Guidance for coding agents and contributors working in this repository.

## What this is

`ocis-agent` is an always-on personal agent for ownCloud Infinite Scale (oCIS): a Go
sidecar (`backend/`) next to oCIS plus an oCIS Web extension (`frontend/`, `web-app-agent`).

Design notes are maintained outside the repository, so this file and the code state
everything a contributor needs.

## Layout

- `backend/` Go module `github.com/owncloud/ocis-agent/backend` (`cmd/`, `internal/`)
- `frontend/` Vue 3 + TypeScript extension, built with `@ownclouders/extension-sdk`
- `docker-compose.yml` + `dev/` dev stack (oCIS + nginx TLS proxy), run from the repo root.
  The host port is `OCIS_DEV_PORT` (default 9200, set in `.env`), so the stack is at
  `https://localhost:$OCIS_DEV_PORT`. Changing it after the first start needs `docker compose down -v`.
  Demo user `einstein` / `relativity`; the extension manifest is served at `/assets/apps/agent/manifest.json`.

## Commands

- `make backend-vet backend-test` Go vet, gofmt check, unit tests
- `make frontend-test frontend-build` Vitest and the production extension build
- `make license-check` REUSE lint (must pass before every commit)
- `make dev-up dev-down` local oCIS stack (after the first `make dev-up`, `docker compose up -d` also works; a fresh clone first needs `dev/certs` and `frontend/dist`, which `make dev-up` creates, and creating them via a bare compose run leaves root-owned bind-mount directories on Linux); `make sidecar` runs the sidecar against the dev stack; `make spike` runs a probe of app-token renewal, SSE events and WebDAV preconditions against the dev stack

## Conventions

- Test first. Every behavior change has a failing test before the code that fixes it.
- Go: standard library first, no cgo, errors wrapped with context and never containing secrets.
- Commits and PR titles are Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`,
  `refactor:`, `test:`, `ci:`, `perf:`; breaking changes use `!` or a `BREAKING CHANGE:`
  footer). The changelog is generated from them, so write subjects as changelog entries.
- Sign off (`git commit -s`) and GPG-sign every commit.
- Licensing is declared centrally in `REUSE.toml` (Apache-2.0). Do not add per-file headers.
  There is no hand-written changelog file.
- A local oCIS checkout (if present) is a read-only reference; never modify it.

## Invariants (do not break)

1. The sidecar's HTTP API lives under `/agent-api/`. Never serve or route `/agent/`: those
   are the extension's UI routes and must reach oCIS Web.
2. The approval gate and all policy are enforced server-side, in Go, from the database.
   File contents, file names, memory, connector output and model text are untrusted data;
   no text may change policy, and destructive or outward actions always need approval.
3. `AGENT_MASTER_KEY` (base64, 32 bytes) comes only from the environment. The sidecar refuses
   to start without it. Never log, print or commit app tokens, the master key or LLM keys.
4. The sidecar talks to oCIS only through public HTTP APIs (Graph, WebDAV, auth-app, SSE).
   No changes to oCIS itself.
5. Approvals, policy changes, enrolment, incident revoke and unenroll require the user's OIDC token,
   never the agent's app token. Only renewing or rotating the agent's own token may use the app token.

## Verified oCIS behaviour (measured on the dev stack; identical on oCIS 8.2.1 and 9.0.0-rc.1, the pinned tag)

- `POST /auth-app/tokens` authenticated with an app token itself succeeds and returns a usable new token, and tokens are
  independent (deleting one leaves the others valid). A stolen token can therefore renew itself whatever the sidecar does.
- Policy for v1: the sidecar renews its own token in the background (below 50% of the TTL, default `AGENT_TOKEN_TTL`
  72h): create with the current token and a unique label `ocis-agent-<instanceId>-<nonce>`, verify with Graph `/me`, swap
  transactionally, then delete the old token. Safeguards: unenroll deletes every token with this instance's label prefix;
  a separately confirmed incident revoke deletes all of the user's app tokens; every renewal and sweep lists the tokens
  and pauses the agent (`token_anomaly`) on an unknown token with our prefix; the agent pauses itself (`dormant`) when
  the user has not been seen for `AGENT_DORMANCY_DAYS` (default 30). Never log or audit token or hash values.
- `DELETE /auth-app/tokens?token=` needs the hash returned in the `token` field of `GET /auth-app/tokens` (find it by
  label and created date), not the plaintext token (plaintext gives 500); with the hash it returns 200 and the token then gets 401.
- The SSE endpoint `/ocs/v2.php/apps/notifications/api/v1/notifications/sse` accepts app-token Basic auth and emits
  `folder-created`, `postprocessing-finished` (upload), `item-renamed` (move) and `item-trashed` (delete) for the
  user's file operations; the data carries ids and etag but no path. Polling is the primary file-change trigger in v1; SSE only speeds it up.
- WebDAV preconditions: `If-None-Match: *` on PUT is ignored (204, the file is overwritten); `If-Match` on PUT is
  enforced but returns 409 (treat 409 and 412 as conflict); `If-Match` on DELETE is ignored (204); `Overwrite: F` on
  MOVE and COPY is enforced (412).
- Decision: write tools stat before creating and refuse if the target exists, and `files.delete` PROPFINDs and compares
  the ETag to the approved one itself before deleting (deletes go to the oCIS trash); the remaining race window is an
  accepted residual risk.
- Decision: no new notification channel (no email, webhook or oCIS notification hack). When the agent pauses itself, the
  reason is surfaced only inside the extension (status endpoint, banner, audit log).
- oCIS has no per-user notification create API (only admin or secret gated global notifications).
