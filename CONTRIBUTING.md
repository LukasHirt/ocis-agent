# Contributing

Thank you for your interest in contributing to this project!

Please read the full contributing guidelines at:
**<https://owncloud.com/contribute/>**

## About this repository

This repository builds the official **oCIS Agent** Docker image
([`owncloud/ocis-agent`](https://hub.docker.com/r/owncloud/ocis-agent)) — the
always-on personal agent sidecar for [ownCloud Infinite Scale](https://github.com/owncloud/ocis).
It is not the ocis-agent source code — it builds the image from source at each
signed upstream release tag via a multi-stage Dockerfile. Bug reports and feature
requests for the application itself belong in the upstream
[`owncloud/ocis-agent`](https://github.com/owncloud/ocis-agent) repository;
use this repository's issues only for the Docker packaging. See the
[README](README.md) for build details, supported tags and usage, and
[agents.md](agents.md) for how CI decides what to build.

## Pull requests

- **Rebase Early, Rebase Often!** We use a rebase workflow. Rebase on the target
  branch before submitting a PR; do not create merge commits.
- **Signed commits**: All commits **must** be PGP/GPG signed. See
  [GitHub's signing guide](https://docs.github.com/en/authentication/managing-commit-signature-verification).
- **DCO Sign-off**: Every commit must carry a `Signed-off-by` line:
  ```
  git commit -S -s -m "your commit message"
  ```
- **Conventional Commits**: PR titles must follow the
  [Conventional Commits](https://www.conventionalcommits.org/) format — this is
  enforced by CI, and the PR title becomes the squash-merge commit message.
- **GitHub Actions Policy**: Workflows may only use actions that are (a) owned by
  `owncloud`, (b) created by GitHub (`actions/*`), or (c) verified in the GitHub
  Marketplace. Pin all actions to their full commit SHA.
- **Scripts**: Changes under `scripts/` must keep `shellcheck` clean and
  `scripts/test-resolve-pending-releases.sh` passing; CI runs both.
