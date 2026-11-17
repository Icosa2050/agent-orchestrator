#!/usr/bin/env bash
#
# Fresh-machine install check. The Dockerfile installs `ao` on PATH in a clean
# image and runs this; it proves a freshly installed binary actually works on a
# machine with no Go toolchain and no developer state. The COMPREHENSIVE,
# cross-platform behavioural suite lives in Go (backend/internal/cli/e2e_test.go,
# `go test -tags e2e`); this stays deliberately small and linear.

set -euo pipefail

AO_BIN="${AO_BIN:-ao}"
tmp="$(mktemp -d)"
export AO_RUN_FILE="$tmp/running.json"
export AO_DATA_DIR="$tmp/data"
trap 'rm -rf "$tmp"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

echo "ao binary : $(command -v "$AO_BIN")"
"$AO_BIN" version            >/dev/null || fail "version"
"$AO_BIN" doctor             >/dev/null || fail "doctor"

# `ao start` is now the desktop-app launcher: it resolves an installed app or
# fetches the release, then opens it (it no longer runs a daemon). On a fresh
# container there is no installed app. Local distribution must refuse acquisition
# clearly rather than downloading a release or starting a daemon.
if err="$("$AO_BIN" start 2>&1)"; then
  fail "start unexpectedly succeeded on a fresh machine with no installed app"
fi
echo "$err" | grep -qF "local Icosa Orchestrator app is not installed; automatic acquisition is disabled for this local build" || fail "start did not fail with a clear error; got: $err"

echo "fresh-install check: OK"
