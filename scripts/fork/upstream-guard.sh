#!/usr/bin/env bash
# Fork guard (Icosa2050): report what an upstream range changes in the areas
# this fork must keep working locally. See FORK.md.
#
# Usage: scripts/fork/upstream-guard.sh [RANGE]
#   RANGE   BASE..HEAD or BASE...HEAD (default: origin/main..upstream/main).
#           Changes are diffed from merge-base(BASE, HEAD) to HEAD, i.e. what
#           merging HEAD into BASE would bring in.
#
# Output is Markdown (usable as a GitHub job summary). Exit codes:
#   0  nothing protected touched
#   1  usage or git error
#   2  needs human review (the full report is still printed)
#
# Local git only; never touches the network. Set GUARD_MAX_LINES to change
# the per-section line cap (default 200).

set -euo pipefail

max_lines=${GUARD_MAX_LINES:-200}
range=${1:-origin/main..upstream/main}

case "$range" in
	-h|--help)
		sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*...*) base=${range%%...*}; head=${range#*...} ;;
	*..*) base=${range%%..*}; head=${range#*..} ;;
	*) echo "upstream-guard: RANGE must be BASE..HEAD or BASE...HEAD, got '$range'" >&2; exit 1 ;;
esac
base=${base:-HEAD}
head=${head:-HEAD}

git rev-parse --git-dir >/dev/null 2>&1 || { echo "upstream-guard: not inside a git repository" >&2; exit 1; }
base_sha=$(git rev-parse --verify --quiet "$base^{commit}") || { echo "upstream-guard: unknown revision '$base'" >&2; exit 1; }
head_sha=$(git rev-parse --verify --quiet "$head^{commit}") || { echo "upstream-guard: unknown revision '$head'" >&2; exit 1; }
merge_base=$(git merge-base "$base_sha" "$head_sha") || { echo "upstream-guard: no merge base between '$base' and '$head'" >&2; exit 1; }

# Paths the fork protects (prefix match, so a stem also covers its tests).
protected_paths='
frontend/src/main/remotes-
frontend/src/main/remote-proxy
frontend/src/renderer/components/settings/RemoteHostsSettings
frontend/src/renderer/components/RemoteHostsSection
frontend/src/renderer/hooks/useRemoteHosts
frontend/src/renderer/lib/account-remote-hosts
frontend/src/renderer/routes/_shell.tsx
frontend/src/main/telemetry-identity
frontend/src/main/sentry-main
frontend/src/shared/sentry-config
frontend/src/main.ts
backend/internal/httpd/lan_
backend/internal/httpd/mobile_connect
backend/internal/httpd/auth_test.go
backend/internal/httpd/controllers/mobile
backend/internal/cli/remote_host
backend/internal/daemon/remote_host_directory
backend/internal/mobilebridge/
backend/internal/adapters/telemetry/optout
backend/internal/adapters/telemetry/identity
backend/internal/observe/sentryobs/
backend/internal/config/config
docs/self-hosted-remote.md
FORK.md
scripts/fork/
.github/workflows/fork-guard.yml
'

# Files carrying fork-only fixes; an upstream change there can drop the fix
# while resolving a merge.
fork_feature_paths='
backend/internal/adapters/chatdriver/acp/conversation.go
backend/internal/service/chat/controller
backend/internal/adapters/chatdriver/kimiacp/
backend/internal/adapters/agent/kimi/
backend/internal/session_manager/chat_spawn.go
backend/internal/adapters/chatdriver/deepseekharnessacp/
backend/internal/service/automation/scheduler
backend/internal/storage/sqlite/queries/automations.sql
backend/internal/storage/sqlite/gen/automations.sql.go
backend/internal/storage/sqlite/store/automation_store
frontend/src/main/shell-crash-recovery
frontend/scripts/build-acp-runtime
'

path_keywords='remote|pair|listen|proxy|account|cloud|auth|tunnel|telemetry|sentry|posthog|dsn'
remote_code='remote|pair|listen|lan_|proxy|tunnel|mobile'
remote_gating='requireaccount|accountuserid|requiresignin|requireauth|ensureaccount|ensuresignedin|issignedin|signedin|sign in to ao cloud|signin|requirecloud|cloudaccount|accountrequired'
global_gating='requireaccount|requiresignin|requirecloud|ensuresignedin|accountrequired|sign in to ao cloud'
outbound='https://|wss://'
# Regexes below are matched by awk against lowercased text; they avoid
# backslash escapes because awk -v would rewrite them.
str="[\"'\`][^\"'\`]+"
secrets="dsn[^a-z]*[:=][[:space:]]*$str|ingest([.][a-z]+)?[.]sentry[.]io|phc_[a-z0-9]{8,}|posthog[a-z_]*(key|host)[^a-z]*[:=][[:space:]]*$str|api[_-]?key[^a-z]*[:=][[:space:]]*$str|default_[a-z_]*(dsn|key|token)"
optout="telemetry_opt_out|optoutfile|isdev[[:space:]]*[?][[:space:]]*[\"'\`]{2}"

work=$(mktemp -d "${TMPDIR:-/tmp}/upstream-guard.XXXXXX")
trap 'rm -rf "$work"' EXIT

git diff --name-status -M --no-color --no-ext-diff "$merge_base" "$head_sha" >"$work/status"
# Normalise renames to "R<TAB>new<TAB>old" so column 2 is always the current path.
awk -F'\t' '{ if ($1 ~ /^[RC]/) print substr($1,1,1) "\t" $3 "\t" $2; else print $1 "\t" $2 }' "$work/status" >"$work/files"

# One row per added (+) or removed (-) line: sign<TAB>path<TAB>line<TAB>text.
# Header lines are only parsed before the first hunk, so a removed SQL comment
# ("-- x" -> "--- x") is not mistaken for a file header.
git diff -U0 -M --no-color --no-ext-diff "$merge_base" "$head_sha" | awk '
	/^diff --git / { path = ""; inhdr = 1; next }
	inhdr && /^--- / { old = substr($0, 5); sub(/^a\//, "", old); next }
	inhdr && /^\+\+\+ / { path = substr($0, 5); sub(/^b\//, "", path); if (path == "/dev/null") path = old; next }
	inhdr && !/^@@ / { next }
	/^@@ / {
		inhdr = 0
		split($2, o, ","); split($3, n, ",")
		oline = substr(o[1], 2) + 0; nline = substr(n[1], 2) + 0
		next
	}
	path == "" { next }
	/^\+/ { printf "+\t%s\t%d\t%s\n", path, nline, substr($0, 2); nline++; next }
	/^-/ { printf "-\t%s\t%d\t%s\n", path, oline, substr($0, 2); oline++; next }
' >"$work/lines"

prefix_match() { # stdin: paths; $1: newline-separated prefixes
	# BSD awk rejects newlines in -v values; pass the list through the environment.
	GUARD_PREFIXES=$1 awk 'BEGIN { n = split(ENVIRON["GUARD_PREFIXES"], p, "\n") }
		{ for (i = 1; i <= n; i++) if (p[i] != "" && index($0, p[i]) == 1) { print; next } }'
}

# Print stdin capped to max_lines inside a code block; returns 1 if empty.
section_body() {
	local file=$work/section count
	cat >"$file"
	count=$(wc -l <"$file" | tr -d ' ')
	if [ "$count" -eq 0 ]; then
		echo "_none_"
		return 1
	fi
	echo '```'
	head -n "$max_lines" "$file"
	if [ "$count" -gt "$max_lines" ]; then echo "... $((count - max_lines)) more (raise GUARD_MAX_LINES)"; fi
	echo '```'
	return 0
}

flag() { # $1: section title; stdin: findings
	echo
	echo "### $1"
	echo
	# Sections run at the end of pipelines (subshells), so record via a file.
	if section_body; then echo "$1" >>"$work/flagged"; fi
}

info() { # like flag, but never counts as a finding
	echo
	echo "### $1"
	echo
	section_body || true
}

# Source files only for line scans: skip docs, tests, fixtures, lockfiles and
# the guard itself (its patterns would match themselves).
scan_lines() { # $1: sign; $2: path regex (lowercased, "" = any); $3: text regex (lowercased)
	awk -F'\t' -v sign="$1" -v pre="$2" -v re="$3" '
		$1 != sign { next }
		{ p = tolower($2) }
		p ~ /(^|\/)docs\/|\.mdx?$|_test\.go$|\.test\.[a-z]+$|\.spec\.[a-z]+$|(^|\/)testdata\/|(^|\/)e2e\/|(^|\/)__fixtures__\/|lock\.json$|\.lock$|go\.sum$|(^|\/)i18n\/|(^|\/)translations\/|^scripts\/fork\/|^fork\.md$|^\.github\/workflows\/fork-guard\.yml$/ { next }
		pre != "" && p !~ pre { next }
		{ t = tolower($0); sub(/^[^\t]*\t[^\t]*\t[^\t]*\t/, "", t) }
		t ~ re { line = $0; sub(/^[^\t]*\t[^\t]*\t[^\t]*\t/, "", line); sub(/^[[:space:]]+/, "", line); printf "%s:%s: %s\n", $2, $3, substr(line, 1, 200) }
	' "$work/lines"
}

commit_count=$(git rev-list --count --no-merges "$merge_base..$head_sha")
merge_count=$(git rev-list --count --merges "$merge_base..$head_sha")
file_count=$(wc -l <"$work/files" | tr -d ' ')

echo "## Fork guard: upstream review"
echo
echo "| | |"
echo "|---|---|"
echo "| Range | \`$range\` |"
echo "| Base | \`$(git rev-parse --short=10 "$base_sha")\` |"
echo "| Head | \`$(git rev-parse --short=10 "$head_sha")\` |"
echo "| Merge base | \`$(git rev-parse --short=10 "$merge_base")\` |"
echo "| Commits | $commit_count (+ $merge_count merges) |"
echo "| Files changed | $file_count |"

git log --no-merges --date=short --format='%h %ad %<(18,trunc)%an %s' "$merge_base..$head_sha" | info "Commits"

awk -F'\t' '$1 == "D" { print $2 }' "$work/files" | prefix_match "$protected_paths" | sed 's/^/DELETED /' >"$work/deleted"
flag "DELETED protected files (blocker unless replaced)" <"$work/deleted"

cut -f2 "$work/files" | prefix_match "$protected_paths" >"$work/protected"
awk -F'\t' 'NR == FNR { s[$2] = $1; next } { print s[$0] "  " $0 }' "$work/files" "$work/protected" |
	flag "Protected paths (local pairing, LAN listener, remote proxy, telemetry opt-out, Sentry DSN, fork guard)"

cut -f2 "$work/files" | prefix_match "$fork_feature_paths" |
	flag "Fork-only fixes touched (check the fix survives the merge)"

cut -f2 "$work/files" | awk -v re="$path_keywords" 'tolower($0) ~ re' | grep -vxF -f "$work/protected" 2>/dev/null |
	flag "Other paths matching remote|pair|listen|proxy|account|cloud|auth|tunnel|telemetry|sentry|posthog|dsn" || true

scan_lines "+" "$remote_code" "$remote_gating" |
	flag "Added account / sign-in gating in remote code"

scan_lines "+" "" "$global_gating" | awk -F: -v re="$remote_code" 'tolower($1) !~ re' |
	flag "Added account / sign-in gating elsewhere"

scan_lines "+" "" "$outbound" |
	flag "New outbound https:// / wss:// literals (source files)"

scan_lines "+" "" "$secrets" |
	flag "New DSN / API-key / token defaults"

{
	scan_lines "-" "" "$optout" | sed 's/^/removed  /'
	scan_lines "+" "" "$optout" | sed 's/^/added    /'
} | flag "Telemetry opt-out marker / dev-build DSN guard changed"

awk -F'\t' '$2 ~ /(^|\/)migrations\/[^\/]+\.sql$/ { print $1 "  " $2 }' "$work/files" |
	flag "SQLite migrations (new or changed)"

awk -F'\t' '$2 ~ /(^|\/)(package\.json|package-lock\.json|npm-shrinkwrap\.json|pnpm-lock\.yaml|yarn\.lock|go\.mod|go\.sum|flake\.lock)$/ { print $1 "  " $2 }' "$work/files" |
	flag "Package manifests / lockfiles"

awk -F'\t' '$2 ~ /(^|\/)(package\.json|go\.mod)$/ {
		t = $0; sub(/^[^\t]*\t[^\t]*\t[^\t]*\t/, "", t); gsub(/^[[:space:]]+|[[:space:],]+$/, "", t)
		if (t ~ /^"[^"]+"[[:space:]]*:[[:space:]]*"[~^<>=]*[0-9]/ || t ~ /^(require[[:space:]]+)?[a-z0-9.-]+\/[^[:space:]]+[[:space:]]+v[0-9]/) print $1 " " $2 ": " t
	}' "$work/lines" | info "Dependency lines changed in package.json / go.mod"

awk -F'\t' '$2 ~ /^\.github\/workflows\// || $2 == "AGENTS.md" { print $1 "  " $2 }' "$work/files" |
	flag "CI workflows / AGENTS.md (cloud-contract guards, review rules)"

echo
if [ -s "$work/flagged" ]; then
	echo "**Verdict: NEEDS REVIEW**, $(wc -l <"$work/flagged" | tr -d ' ') flagged sections:"
	echo
	sed 's/^/- /' "$work/flagged"
	echo
	echo "Follow the review procedure in FORK.md before syncing."
	exit 2
fi
echo "**Verdict: OK**, nothing protected touched."
exit 0
