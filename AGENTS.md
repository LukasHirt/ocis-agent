# AGENTS.md

Guidance for coding agents and contributors working in this repository.

## What this is

`ocis-agent` is an always-on personal agent for ownCloud Infinite Scale (oCIS): a Go
sidecar (`backend/`) next to oCIS plus an oCIS Web extension (`frontend/`, `web-app-agent`).

Design notes and implementation plans are kept locally by the maintainers (git-ignored
under `docs/superpowers/`) and are not part of the repository, so this file and the code
state everything a contributor needs.

## Layout

- `backend/` Go module `github.com/owncloud/ocis-agent/backend` (`cmd/`, `internal/`)
- `frontend/` Vue 3 + TypeScript extension, built with `@ownclouders/extension-sdk`
- `docker-compose.yml` + `dev/` dev stack (oCIS + nginx TLS proxy on https://localhost:9200), run from the repo root

## Commands

- `make backend-vet backend-test` Go vet, gofmt check, unit tests
- `make frontend-test frontend-build` Vitest and the production extension build
- `make license-check` REUSE lint (must pass before every commit)
- `make dev-up dev-down` local oCIS stack (plain `docker compose up -d` also works from the repo root); `make spike` runs the Q3/Q4 probe

## Conventions

- Test first. Every behavior change has a failing test before the code that fixes it.
- Go: standard library first, no cgo, errors wrapped with context and never containing secrets.
- Commits and PR titles are Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`,
  `refactor:`, `test:`, `ci:`, `perf:`; breaking changes use `!` or a `BREAKING CHANGE:`
  footer). The changelog is generated from them, so write subjects as changelog entries.
- Sign off (`git commit -s`) and GPG-sign every commit.
- Licensing is declared centrally in `REUSE.toml` (Apache-2.0). Do not add per-file headers.
  There is no hand-written changelog file.
- `/Users/lukas.hirt/projects/ocis` (if present) is a read-only reference checkout of oCIS.

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
5. Approvals, policy changes and token operations require the user's OIDC token, never the
   agent's app token.
