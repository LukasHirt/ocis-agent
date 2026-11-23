#!/usr/bin/env bash
# Tests for resolve-pending-releases.sh. No network: every upstream and Docker
# Hub input is supplied through the resolver's test hooks, and tag
# verification is replaced by a stub. Run it directly; CI runs it in the
# `test` job of .github/workflows/main.yml.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESOLVER="${SCRIPT_DIR}/resolve-pending-releases.sh"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Verification stub: prints a fake commit SHA for the tag, and fails (like an
# unsigned tag) for every tag listed in $UNVERIFIED.
STUB="${TMP}/verify-tag"
cat >"$STUB" <<'EOF'
#!/usr/bin/env bash
for t in ${UNVERIFIED:-}; do
  if [ "$t" = "$1" ]; then
    echo "::warning::$1 is not verified (stub)" >&2
    exit 1
  fi
done
echo "sha-of-$1"
EOF
chmod +x "$STUB"

failures=0

lines() { [ "$#" -eq 0 ] && return 0; printf '%s\n' "$@"; }

# expect <name> <mode> <upstream tags> <published tags> <unverified> <expected json>
# Tag lists are space-separated.
expect() {
  local name=$1 mode=$2 upstream=$3 published=$4 unverified=$5 expected=$6 actual
  # shellcheck disable=SC2086 # word splitting of the tag lists is intended
  if ! actual=$(MODE="$mode" \
    UPSTREAM_TAGS="$(lines $upstream)" \
    PUBLISHED_TAGS="$(lines $published)" \
    UPSTREAM_MAIN_SHA=mainsha \
    UNVERIFIED="$unverified" \
    VERIFY_TAG="$STUB" \
    "$RESOLVER" 2>"${TMP}/stderr"); then
    echo "FAIL ${name}: resolver exited non-zero"
    sed 's/^/  /' "${TMP}/stderr"
    failures=$((failures + 1))
    return
  fi
  if [ "$(jq -S -c 'sort_by(.version)' <<<"$actual")" = "$(jq -S -c 'sort_by(.version)' <<<"$expected")" ]; then
    echo "ok ${name}"
  else
    echo "FAIL ${name}"
    echo "  expected: ${expected}"
    echo "  actual:   ${actual}"
    failures=$((failures + 1))
  fi
}

entry() { # entry <version> <latest>
  printf '{"version":"%s","ref":"v%s","sha":"sha-of-v%s","latest":%s}' "$1" "$1" "$1" "$2"
}

expect no_tags_pending_empty pending "" "" "" '[]'

expect pr_mode_no_tags_builds_main pr "" "" "" \
  '[{"version":"main","ref":"main","sha":"mainsha","latest":false}]'

expect two_pending_both_built pending "v0.1.0 v0.2.0" "" "" \
  "[$(entry 0.1.0 false),$(entry 0.2.0 true)]"

expect latest_uses_version_sort pending "v0.9.0 v0.10.0" "" "" \
  "[$(entry 0.9.0 false),$(entry 0.10.0 true)]"

expect published_skipped pending "v0.1.0 v0.2.0" "0.1.0 0.1.0-20261101 latest" "" \
  "[$(entry 0.2.0 true)]"

expect older_missing_not_latest pending "v0.1.0 v0.2.0" "0.2.0 latest" "" \
  "[$(entry 0.1.0 false)]"

expect all_published_pending_empty pending "v0.1.0 v0.2.0" "0.1.0 0.2.0" "" '[]'

expect rebuild_adds_newest rebuild "v0.1.0 v0.2.0" "0.1.0 0.2.0" "" \
  "[$(entry 0.2.0 true)]"

expect rebuild_keeps_pending rebuild "v0.1.0 v0.2.0" "0.2.0" "" \
  "[$(entry 0.1.0 false),$(entry 0.2.0 true)]"

expect pr_mode_tags_builds_newest_only pr "v0.1.0 v0.2.0" "0.1.0 0.2.0" "" \
  "[$(entry 0.2.0 true)]"

expect non_plain_tags_ignored pending "v0.1.0 v0.2.0-rc.1 v1.0 foo 0.3.0 v0.4.0^{}" "" "" \
  "[$(entry 0.1.0 true)]"

expect unverified_tag_skipped pending "v0.1.0 v0.2.0" "" "v0.2.0" \
  "[$(entry 0.1.0 false)]"
if grep -q '::warning::' "${TMP}/stderr"; then
  echo "ok unverified_tag_skipped_warns"
else
  echo "FAIL unverified_tag_skipped_warns: no ::warning:: on stderr"
  failures=$((failures + 1))
fi

# Docker Hub unreachable: the resolver must fail rather than report nothing
# as published (which would rebuild everything) or emit an empty matrix.
if out=$(MODE=pending UPSTREAM_TAGS=v0.1.0 UPSTREAM_MAIN_SHA=mainsha VERIFY_TAG="$STUB" \
  DOCKERHUB_TAGS_URL=http://127.0.0.1:9/ CURL_RETRY=0 \
  "$RESOLVER" 2>"${TMP}/stderr"); then
  echo "FAIL hub_unreachable_fails: resolver succeeded with: ${out}"
  failures=$((failures + 1))
elif [ -n "$out" ]; then
  echo "FAIL hub_unreachable_fails: resolver printed: ${out}"
  failures=$((failures + 1))
else
  echo "ok hub_unreachable_fails"
fi

# A pull request must always build something: when the newest tag does not
# verify, fall back to upstream main instead of an empty matrix.
expect pr_mode_unverified_newest_builds_main pr "v0.1.0 v0.2.0" "0.1.0 0.2.0" "v0.2.0" \
  '[{"version":"main","ref":"main","sha":"mainsha","latest":false}]'

# Docker Hub answers served by a local fixture server: a page that is not the
# expected JSON, or an error after the first page, must fail the run instead
# of making releases look unpublished.
HUB="${TMP}/hub"
mkdir -p "$HUB"
PORT=$((20000 + RANDOM % 20000))
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$HUB" >/dev/null 2>&1 &
SERVER=$!
disown "$SERVER"
trap 'kill "$SERVER" 2>/dev/null || true; rm -rf "$TMP"' EXIT
for _ in $(seq 1 50); do
  curl -s -o /dev/null "http://127.0.0.1:${PORT}/" && break
  sleep 0.1
done
echo '<html>maintenance</html>' >"${HUB}/html"
printf '{"results":[{"name":"0.1.0"}],"next":"http://127.0.0.1:%s/missing"}' "$PORT" >"${HUB}/page1"

hub_fails() { # hub_fails <name> <first page path>
  local name=$1 out
  if out=$(MODE=pending UPSTREAM_TAGS="v0.1.0" UPSTREAM_MAIN_SHA=mainsha VERIFY_TAG="$STUB" \
    DOCKERHUB_TAGS_URL="http://127.0.0.1:${PORT}/$2" CURL_RETRY=0 \
    "$RESOLVER" 2>"${TMP}/stderr"); then
    echo "FAIL ${name}: resolver succeeded with: ${out}"
    failures=$((failures + 1))
  else
    echo "ok ${name}"
  fi
}
# An existing repository without any tag yet is a valid, empty listing.
echo '{"count":0,"next":null,"results":[]}' >"${HUB}/empty"
if out=$(MODE=pending UPSTREAM_TAGS="v0.1.0" UPSTREAM_MAIN_SHA=mainsha VERIFY_TAG="$STUB" \
  DOCKERHUB_TAGS_URL="http://127.0.0.1:${PORT}/empty" CURL_RETRY=0 \
  "$RESOLVER" 2>"${TMP}/stderr") && [ "$out" = "[$(entry 0.1.0 true)]" ]; then
  echo "ok hub_empty_repository_builds_all"
else
  echo "FAIL hub_empty_repository_builds_all: ${out:-<failed>}"
  sed 's/^/  /' "${TMP}/stderr"
  failures=$((failures + 1))
fi
hub_fails hub_malformed_page_fails html
hub_fails hub_error_after_first_page_fails page1

if [ "$failures" -ne 0 ]; then
  echo "${failures} test(s) failed"
  exit 1
fi
echo "all tests passed"
