#!/usr/bin/env bash
# Prints the build matrix for .github/workflows/main.yml as a JSON array of
# {version, ref, sha, latest}, one entry per image to build.
#
# GitHub Actions cannot trigger on a tag pushed to a *different* repository,
# so instead of reacting to upstream release tags, CI polls: it lists the
# upstream owncloud/ocis-agent tags and diffs them against the tags
# already on Docker Hub.
#
# MODE selects what to build:
#   pending  (default) every upstream release not yet on Docker Hub
#   rebuild  the same, plus the newest release even if it is already
#            published, so base-image and toolchain fixes reach it
#   pr       only the newest release, published or not, so a pull request
#            always builds something; upstream main while there is no
#            release or the newest one does not verify
#
# Every missing release is a candidate - there is deliberately no "keep only
# the highest version" reduction, which would permanently drop a release when
# two land in one polling window. Only plain vX.Y.Z tags are releases. Each
# candidate must be an annotated tag with a verified signature
# (verify-tag.sh); one that is not is skipped with a warning, and `latest`
# is never handed to an older release in its place.
#
# Test hooks (defaults in brackets), used by test-resolve-pending-releases.sh:
#   UPSTREAM_TAGS       newline-separated upstream tag names [git ls-remote]
#   UPSTREAM_MAIN_SHA   upstream main HEAD [git ls-remote]
#   PUBLISHED_TAGS      newline-separated Docker Hub tags; when set, even to
#                       an empty string, Docker Hub is not queried
#   DOCKERHUB_TAGS_URL  first page of the Docker Hub tag listing
#   VERIFY_TAG          tag verification command [scripts/verify-tag.sh]
#   CURL_RETRY          curl retries per Docker Hub page [5]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPSTREAM_URL="https://github.com/owncloud/ocis-agent.git"
DOCKER_REPO="owncloud/ocis-agent"
MODE=${MODE:-pending}
VERIFY_TAG=${VERIFY_TAG:-${SCRIPT_DIR}/verify-tag.sh}

case "$MODE" in
  pending | rebuild | pr) ;;
  *)
    echo "::error::unknown MODE '${MODE}' (pending, rebuild or pr)" >&2
    exit 1
    ;;
esac

# 1. Upstream release versions (without the v), sorted ascending.
if [ -z "${UPSTREAM_TAGS+set}" ]; then
  UPSTREAM_TAGS=$(git ls-remote --tags "$UPSTREAM_URL" | cut -f2 | sed -n 's|^refs/tags/||p')
fi
VERSIONS=$(printf '%s\n' "$UPSTREAM_TAGS" | sed -n 's/^v\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)$/\1/p' | sort -uV)
HIGHEST=$(printf '%s\n' "$VERSIONS" | tail -n 1)

# 2. Tags already published on Docker Hub. A missing repository (404 on the
#    first page) means nothing is published yet; any other failure aborts, so
#    an outage can never be mistaken for "nothing published".
published_tags() {
  local url=${DOCKERHUB_TAGS_URL:-https://hub.docker.com/v2/repositories/${DOCKER_REPO}/tags?page_size=100}
  local body status first=true
  body=$(mktemp)
  while [ -n "$url" ] && [ "$url" != null ]; do
    if ! status=$(curl -sS --retry "${CURL_RETRY:-5}" --retry-delay 5 --retry-all-errors \
      -o "$body" -w '%{http_code}' "$url"); then
      echo "::error::could not reach Docker Hub (${url})" >&2
      rm -f "$body"
      return 1
    fi
    if [ "$status" = 404 ] && $first; then
      echo "Docker Hub repository ${DOCKER_REPO} does not exist yet; nothing is published" >&2
      break
    fi
    if [ "$status" != 200 ]; then
      echo "::error::Docker Hub answered HTTP ${status} for ${url}" >&2
      rm -f "$body"
      return 1
    fi
    # -e: a 200 that is not the expected JSON (a maintenance page, a
    # truncated body) must fail too. Command substitution does not inherit
    # errexit, so check each jq call explicitly.
    if ! jq -e '.results | type == "array"' "$body" >/dev/null ||
      ! jq -r '.results[].name' "$body" || ! url=$(jq -r '.next // ""' "$body"); then
      echo "::error::Docker Hub returned an unexpected response for ${url}" >&2
      rm -f "$body"
      return 1
    fi
    first=false
  done
  rm -f "$body"
}
if [ -z "${PUBLISHED_TAGS+set}" ]; then
  PUBLISHED_TAGS=$(published_tags)
fi

# 3. Candidates for this run.
CANDIDATES=""
if [ "$MODE" = pr ]; then
  CANDIDATES=$HIGHEST
else
  while IFS= read -r version; do
    [ -z "$version" ] && continue
    if ! grep -qxF "$version" <<<"$PUBLISHED_TAGS" ||
      { [ "$MODE" = rebuild ] && [ "$version" = "$HIGHEST" ]; }; then
      CANDIDATES+="${version}"$'\n'
    fi
  done <<<"$VERSIONS"
fi

# 4. Verify each candidate and emit the matrix.
MATRIX=$(while IFS= read -r version; do
  [ -z "$version" ] && continue
  sha=$("$VERIFY_TAG" "v${version}") || continue
  latest=false
  [ "$version" = "$HIGHEST" ] && latest=true
  jq -n --arg version "$version" --arg ref "v${version}" --arg sha "$sha" --argjson latest "$latest" \
    '{version: $version, ref: $ref, sha: $sha, latest: $latest}'
done <<<"$CANDIDATES" | jq -s -c .)

# A pull request always builds something, so Dockerfile changes are checked:
# when there is no release, or the newest one does not verify, it builds
# upstream main instead (never pushed).
if [ "$MODE" = pr ] && [ "$MATRIX" = "[]" ]; then
  if [ -z "${UPSTREAM_MAIN_SHA:-}" ]; then
    UPSTREAM_MAIN_SHA=$(git ls-remote "$UPSTREAM_URL" refs/heads/main | cut -f1)
  fi
  if [ -z "$UPSTREAM_MAIN_SHA" ]; then
    echo "::error::could not resolve upstream main" >&2
    exit 1
  fi
  MATRIX=$(jq -n -c --arg sha "$UPSTREAM_MAIN_SHA" '[{version: "main", ref: "main", sha: $sha, latest: false}]')
fi
echo "Pending builds (${MODE}): ${MATRIX}" >&2
echo "$MATRIX"
