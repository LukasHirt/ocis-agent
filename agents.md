# agents.md — ocis-agent

## Repository Overview

This repository builds the official **oCIS Agent** Docker image (`owncloud/ocis-agent`
on Docker Hub) — the always-on personal agent sidecar for ownCloud Infinite Scale. It
does not contain the ocis-agent source code — it builds the sidecar **from source** by
cloning [`owncloud/ocis-agent`](https://github.com/owncloud/ocis-agent) at a signed
release tag and ships a minimal Alpine runtime image. Images are multi-architecture and
built via GitHub Actions.

This repository is the **only** publisher of the image, the same split as oCIS: the
application repository cuts signed release tags and the GitHub release, and this
repository publishes the container image.

- **Classification:** Docker image build (from source)
- **Activity Status:** Active
- **License:** Apache-2.0
- **Language:** Dockerfile, Shell

## Architecture & Key Paths

- `Dockerfile.multiarch` — three-stage build (build context is the repo root, but nothing
  is copied from it):
  - **source** (`golang:*-alpine`) — clones upstream at `${GIT_REF}`, pinned to
    `${GIT_SHA}` when set.
  - **build** (`golang:*-alpine`) — compiles `backend/cmd/ocis-agent` with
    `CGO_ENABLED=0` for `${TARGETARCH}`.
  - **runtime** (`alpine`) — `apk upgrade`, `/usr/local/bin/ocis-agent` as the entrypoint,
    uid/gid 10001, `/data` (0700), `AGENT_DATA_DIR=/data`, `AGENT_HTTP_ADDR=:9300`,
    `AGENT_DEBUG_ADDR=:9301`, ports `9300` and `9301`, a `HEALTHCHECK` on
    `/agent-api/healthz`, `STOPSIGNAL SIGTERM`.
- `scripts/resolve-pending-releases.sh` — decides what CI builds; prints the build
  matrix as JSON.
- `scripts/verify-tag.sh` — checks that an upstream tag is annotated and its signature
  is verified, and prints the commit it points to.
- `scripts/test-resolve-pending-releases.sh` — offline tests for the resolver.
- `.github/workflows/main.yml` — **active** CI: test, resolve, build, scan, smoke test,
  publish.
- `.github/workflows/lint-pr-title.yml` — Conventional-Commit PR-title enforcement
- `.github/dependabot.yml` — weekly GitHub Actions and Docker base-image dependency updates
- `.github/CODEOWNERS` — review ownership
- `.editorconfig` — formatting rules (2-space indent, LF, trailing newline)
- `.trivyignore` — accepted-CVE exclusions for the Trivy scan
- `LICENSE` — Apache-2.0

There is **no `CHANGELOG.md`** in this repository.

## Build & CI

GitHub Actions cannot trigger on a tag pushed to another repository, so `main.yml`
polls. Its `prepare` job runs `scripts/resolve-pending-releases.sh`, which:

1. Lists the upstream tags (`git ls-remote --tags`) and keeps only plain `vX.Y.Z` ones.
2. Lists the tags already on Docker Hub (paginated). A missing Docker Hub repository
  (HTTP 404) counts as "nothing published"; any other failure aborts the run.
3. Picks candidates by `MODE`, verifies each with `scripts/verify-tag.sh`, and prints
  `[{version, ref, sha, latest}, …]`. `latest` is true only for the highest upstream
  version (`sort -V`).

| Trigger | `MODE` | Builds |
|---------|--------|--------|
| `schedule` `0 */6 * * *`, `workflow_dispatch` without `rebuild-newest` | `pending` | every release not yet on Docker Hub |
| `push` to `main`, `schedule` `0 3 * * 0`, `workflow_dispatch` with `rebuild-newest` | `rebuild` | the same, plus the newest release |
| `pull_request` | `pr` | the newest release, or upstream `main` while there is none or it does not verify; never pushed |

`build` calls the reusable `docker-build-native.yml` workflow from
[`owncloud-docker/ubuntu`](https://github.com/owncloud-docker/ubuntu) per matrix leg
(`fail-fast: false`). Each leg is tagged `X.Y.Z` and `X.Y.Z-YYYYMMDD`, plus `latest` for
the highest release. It is scanned with Trivy (`.trivyignore`, `exit-code: 1`) and
smoke-tested by starting the default entrypoint with `AGENT_ROLES=api` (the worker role
refuses to start without a model endpoint), a throwaway all-zero `AGENT_MASTER_KEY` and
`AGENT_OCIS_URL=https://ocis.invalid`, and polling
`http://localhost:9300/agent-api/healthz`. There is no version assertion, because the
binary reports no version. On non-PR events the image is pushed to Docker Hub and the
README is synced as the image description.

The resolver's inputs can be replaced for testing: `UPSTREAM_TAGS`,
`UPSTREAM_MAIN_SHA`, `PUBLISHED_TAGS`, `DOCKERHUB_TAGS_URL`, `VERIFY_TAG`,
`CURL_RETRY` (see the script header).

```bash
scripts/test-resolve-pending-releases.sh                               # offline tests
MODE=pending scripts/resolve-pending-releases.sh                       # what CI would build now
GH_TOKEN=$(gh auth token) MODE=pr scripts/resolve-pending-releases.sh  # what a PR would build
```

To build and smoke-test locally:

```bash
docker buildx build --load -f Dockerfile.multiarch --build-arg GIT_REF=main -t ocis-agent:test .
docker run --rm -p 9300:9300 -e AGENT_ROLES=api \
  -e AGENT_MASTER_KEY="$(openssl rand -base64 32)" -e AGENT_OCIS_URL=https://ocis.invalid \
  ocis-agent:test
curl -f http://localhost:9300/agent-api/healthz
```

## Deviations from the Sibling Repositories

- **Release polling instead of a hand-edited matrix.** New upstream tags are built
  automatically, as in `web-extensions` and `ocis-groupware`, rather than added to
  `main.yml` by a PR, as in `ocis`.
- **uid 10001 instead of 1000.** Upstream's documentation, its Compose example and
  existing `/data` volumes all assume uid/gid 10001.
- **Sidecar only.** The agent's ownCloud Web extension (`frontend/`, `web-app-agent`)
  is a separate bundle and is not packaged by this repository.
- **No rolling or nightly image.** Only signed release tags are published. Upstream
  `main` is built only in pull requests, never pushed. The weekly rebuild of the newest
  release picks up Alpine package fixes via `apk upgrade`.
- **No `vN/` directory.** Everything is 0.x and only the newest release is rebuilt, so
  there is one root `Dockerfile.multiarch` and `.trivyignore`. Add a `vN/` directory
  only when a major line needs a different build.

## Development Conventions

- **No CHANGELOG** — do not create one.
- Conventional-Commit PR titles, enforced by `lint-pr-title.yml`.
- `.editorconfig` governs formatting; `LICENSE` is exempt from the indent rules.
- GitHub Actions are pinned to full commit SHAs.
- Workflows declare a least-privilege `permissions:` block.
- Scripts must stay `shellcheck`-clean; CI runs it.
- Bug reports for the application itself go upstream to
  [`owncloud/ocis-agent`](https://github.com/owncloud/ocis-agent); this repo tracks only
  the Docker packaging.

## OSPO Policy Constraints

### GitHub Actions
- **Only** use actions owned by `owncloud`, created by GitHub (`actions/*`),
  verified on the GitHub Marketplace, or verified by the ownCloud Maintainers.
- Pin all actions to their full commit SHA (not tags): `uses: actions/checkout@<SHA> # vX.Y.Z`.
- Never introduce actions from unverified third parties.

### Dependency Management
- Dependabot covers both GitHub Actions and Docker base-image updates for this
  repo (`.github/dependabot.yml`) — Renovate is not used here.
- Dependabot PRs do not auto-merge; review and merge them as part of regular
  maintenance. Merging one rebuilds and re-pushes the newest release.
- Base images in `Dockerfile.multiarch` must remain pinned as `tag@sha256:<digest>`;
  Dependabot updates the digest in place. A stale `golang` digest surfaces as Go
  `stdlib` CVEs failing the Trivy gate; the fix is bumping the digest, not adding
  `.trivyignore` entries.
- A HIGH/CRITICAL CVE in a Go module compiled into the binary blocks the build until
  upstream ships a fix. Only when upstream cannot fix it promptly does it go into
  `.trivyignore`, with the CVE, the reason and the upstream issue.

### Git Workflow
- **Rebase policy**: Always rebase; never create merge commits.
- **Signed commits**: All commits **must** be PGP/GPG signed (`git commit -S`).
- **DCO sign-off**: Every commit needs a `Signed-off-by` line (`git commit -s`).
- **Conventional Commits & Squash Merge**: PR titles must follow
  [Conventional Commits](https://www.conventionalcommits.org/); the PR title
  becomes the squash-merge commit message and is enforced by CI.

## Context for AI Agents

- This is a Docker-image build repo that compiles the ocis-agent sidecar from source —
  not the application codebase. Application changes belong upstream.
- The resolver is the only thing that decides what gets built: every verified upstream
  release missing from Docker Hub. Do not reintroduce a "newest version only"
  reduction for pending builds — it drops a release when two land in one polling
  window.
- Do not change the uid/gid (10001), the data path (`/data`), the ports, the
  `AGENT_*_ADDR` defaults the health check relies on, or the entrypoint path: upstream's
  docs, Compose example and existing volumes depend on them.
- Never bake `AGENT_MASTER_KEY` or any other secret into the image. The smoke test's
  all-zero key is a public throwaway for the test container only.
- The README is published verbatim as the Docker Hub image description — keep it
  accurate and self-contained.
- License is **Apache-2.0**, the OSPO's ecosystem-wide target; no relicensing needed.
