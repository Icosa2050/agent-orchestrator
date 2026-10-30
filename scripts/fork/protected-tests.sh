#!/usr/bin/env bash
# Fork guard (Icosa2050): run the tests that protect local-only operation
# (manual remote pairing, remote proxy, LAN listener, remote-host CLI,
# telemetry opt-out, empty dev Sentry DSNs). See FORK.md.
#
# Usage: scripts/fork/protected-tests.sh [frontend|backend|all]   (default all)
#
# A selection that disappears or shrinks fails, not just a failing test: every
# listed vitest file must exist and pass at least its floor of tests, and every
# go test selection must list at least its floor of tests before it runs.
# Floors are the counts when the guard was added. Raise them when tests are
# added; lower one only in a reviewed PR that explains the removal.
#
# Expects frontend dependencies installed (npm ci --ignore-scripts).

set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
mode=${1:-all}

# path (relative to frontend/) and minimum passing tests.
frontend_tests='
src/main/remotes-main.test.ts 41
src/main/remotes-ipc.test.ts 8
src/main/remotes-store.test.ts 22
src/main/remote-proxy.test.ts 70
src/renderer/components/settings/RemoteHostsSettings.test.tsx 12
src/renderer/components/RemoteHostsSection.test.tsx 1
src/renderer/hooks/useRemoteHosts.test.tsx 15
src/renderer/hooks/useRemoteHosts.self.test.tsx 3
src/shared/sentry-config.test.ts 3
src/main/sentry-main.test.ts 17
src/main/telemetry-identity.test.ts 20
'

# package (relative to backend/), -run regex (no spaces) and minimum listed tests.
backend_tests='
./internal/httpd ^(TestLAN|TestMobileLAN|TestAccountTokenIssuanceReturnsThroughRealLANListener) 29
./internal/httpd/controllers ^(TestLANOnly|TestTunnelOnly|TestMobileStatus) 19
./internal/cli ^TestRemoteHost 7
./internal/daemon ^TestPublishRemoteHost 1
./internal/mobilebridge . 116
./internal/adapters/telemetry OptOut 4
'

fail() { echo "protected-tests: $*" >&2; exit 1; }

run_frontend() {
	cd "$root/frontend"
	local files=() report
	while read -r file min; do
		[ -n "$file" ] || continue
		[ -f "$file" ] || fail "missing protected test file frontend/$file"
		files+=("$file")
	done <<<"$frontend_tests"
	report=$(mktemp "${TMPDIR:-/tmp}/protected-vitest.XXXXXX")
	npx vitest run --config vite.renderer.config.ts --reporter=default --reporter=json --outputFile.json="$report" "${files[@]}"
	# vitest's file arguments are substring filters, so check every file ran
	# and kept at least its floor of passing tests.
	# shellcheck disable=SC2016 # JavaScript template literals, not shell.
	GUARD_FLOORS=$frontend_tests node -e '
		const report = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
		const passed = new Map(report.testResults.map((r) => [
			require("path").relative(process.cwd(), r.name),
			r.assertionResults.filter((a) => a.status === "passed").length,
		]));
		let bad = 0;
		for (const line of process.env.GUARD_FLOORS.split("\n")) {
			const [file, min] = line.trim().split(/\s+/);
			if (!file) continue;
			const got = passed.get(file) ?? 0;
			const ok = got >= Number(min);
			console.log(`${ok ? "ok  " : "FAIL"} ${got} passed (floor ${min}) frontend/${file}`);
			if (!ok) bad++;
		}
		process.exit(bad ? 1 : 0);
	' "$report" || fail "a protected vitest file ran fewer tests than its floor"
	rm -f "$report"
}

run_backend() {
	cd "$root/backend"
	local pkg re min count
	while read -r pkg re min; do
		[ -n "$pkg" ] || continue
		# go test -run passes with "no tests to run"; count the selection first.
		count=$(go test -list "$re" "$pkg" | grep -cE '^(Test|Example|Fuzz)' || true)
		echo "go test $pkg -run '$re': $count tests (floor $min)"
		[ "$count" -ge "$min" ] || fail "$pkg -run '$re' selects $count tests, below the floor of $min"
		go test -count=1 -run "$re" "$pkg"
	done <<<"$backend_tests"
}

case "$mode" in
	frontend) run_frontend ;;
	backend) run_backend ;;
	all) run_frontend; run_backend ;;
	*) fail "usage: $0 [frontend|backend|all]" ;;
esac
