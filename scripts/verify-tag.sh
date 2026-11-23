#!/usr/bin/env bash
# Usage: scripts/verify-tag.sh <tag>
#
# Checks that <tag> in the upstream repo is an annotated tag whose signature
# GitHub verifies - the same rule upstream's own release workflow enforces -
# and prints the commit it points to. Exits 1 with a ::warning:: on stderr
# otherwise, so resolve-pending-releases.sh can skip the tag and carry on.
#
# Needs GH_TOKEN (any token; the upstream repo is public). Override the repo
# with UPSTREAM_REPO_SLUG.
set -euo pipefail

TAG=${1:?usage: verify-tag.sh <tag>}
SLUG=${UPSTREAM_REPO_SLUG:-owncloud/ocis-agent}

warn() {
  echo "::warning::${TAG} is skipped: $*" >&2
  exit 1
}

ref=$(gh api "repos/${SLUG}/git/ref/tags/${TAG}" 2>/dev/null) ||
  warn "the tag does not exist in ${SLUG} or the API call failed"
[ "$(jq -r .object.type <<<"$ref")" = tag ] ||
  warn "it is a lightweight tag, not an annotated one"

tag=$(gh api "repos/${SLUG}/git/tags/$(jq -r .object.sha <<<"$ref")" 2>/dev/null) ||
  warn "the tag object could not be read"
[ "$(jq -r .verification.verified <<<"$tag")" = true ] ||
  warn "its signature is not verified ($(jq -r .verification.reason <<<"$tag"))"
[ "$(jq -r .object.type <<<"$tag")" = commit ] ||
  warn "it does not point at a commit"

jq -r .object.sha <<<"$tag"
