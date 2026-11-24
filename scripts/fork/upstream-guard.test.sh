#!/usr/bin/env bash
# Self-test for upstream-guard.sh (Icosa2050, see FORK.md): a merge or PR that
# drops a "Fork: fleet seam" marker must be flagged; an upstream-only range and
# a change that keeps the markers must not be.
#
# Usage: scripts/fork/upstream-guard.test.sh   (local git only)

set -euo pipefail

guard=$(cd "$(dirname "$0")" && pwd)/upstream-guard.sh
repo=$(mktemp -d "${TMPDIR:-/tmp}/guard-test.XXXXXX")
trap 'rm -rf "$repo" "$repo.out"' EXIT
section='Fork fleet seam markers removed'

fail() { echo "upstream-guard.test: $*" >&2; exit 1; }

g() { git -C "$repo" -c user.name=guard -c user.email=guard@example.invalid -c commit.gpgsign=false "$@"; }

# run RANGE -> prints the guard's exit code; output in $repo.out
run() {
	local code=0
	(cd "$repo" && "$guard" "$1") >"$repo.out" 2>&1 || code=$?
	echo "$code"
}

g init -q -b main
mkdir -p "$repo/backend/internal/httpd"
printf 'package httpd\n' >"$repo/backend/internal/httpd/router.go"
g add -A && g commit -qm upstream
upstream=$(g rev-parse HEAD)

# Fork main adds a seam.
printf 'package httpd\n\n// Fork: fleet seam (FORK.md).\nfunc mount() {}\n' >"$repo/backend/internal/httpd/router.go"
g commit -qam "fork seam"
fork=$(g rev-parse HEAD)

# Upstream moves on without the seam: reviewing upstream is not a seam finding.
g checkout -q -b upstream "$upstream"
printf 'package httpd\n\nfunc other() {}\n' >"$repo/backend/internal/httpd/other.go"
g add -A && g commit -qm "upstream change"
run "$fork..upstream" >/dev/null
grep -q "$section" "$repo.out" || fail "section missing from the report"
grep -A3 "### $section" "$repo.out" | grep -q '_none_' || fail "upstream-only range flagged a seam: $(cat "$repo.out")"

# A sync merge that keeps the seam is clean for seams.
g checkout -q -b keep "$fork"
g merge -q --no-edit upstream
run "$fork..keep" >/dev/null
grep -A3 "### $section" "$repo.out" | grep -q '_none_' || fail "kept seam flagged: $(cat "$repo.out")"

# A sync merge (patched copy) that drops the seam is flagged as needing review.
g checkout -q -b drop "$fork"
g merge -q --no-edit upstream
printf 'package httpd\n\nfunc mount() {}\n' >"$repo/backend/internal/httpd/router.go"
g commit -qam "resolve conflict, dropping the seam"
code=$(run "$fork..drop")
[ "$code" = 2 ] || fail "dropped seam: exit $code, want 2"
grep -A3 "### $section" "$repo.out" | grep -q 'backend/internal/httpd/router.go: 1 -> 0 markers' || fail "dropped seam not reported: $(cat "$repo.out")"

# Deleting the file is flagged too.
g checkout -q -b delete "$fork"
g rm -q backend/internal/httpd/router.go && g commit -qm "delete"
run "$fork..delete" >/dev/null
grep -A3 "### $section" "$repo.out" | grep -q 'router.go: 1 -> 0 markers' || fail "deleted seam file not reported: $(cat "$repo.out")"

rm -f "$repo.out"
echo "upstream-guard.test: ok"
