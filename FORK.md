# Icosa2050 fork of Agent Orchestrator

This file exists only in the fork `Icosa2050/agent-orchestrator`. Upstream is
`OrchestratorInc/agent-orchestrator` (remote `upstream`); the fork is `origin`.

## Why the fork exists

Upstream is moving functionality into AO Cloud, a paid, closed service: cloud
sync is limited to two computers, the cloud control plane moved to a private
repository (upstream #7352), and upstream CI now checks the cloud contract
(upstream #7356). This fork keeps AO fully usable **locally, forever**: without
an AO Cloud account, without signing in, and with credentials that never leave
the machine.

**Owner rule:** no upstream sync may remove, gate or weaken local operation.
Every sync is reviewed with the guard below. A conflicting upstream change is
resolved in a sync PR that keeps the local capability. The owner decides; there
are no blind syncs.

## Protected capabilities

| Capability | Code paths | Tests that must pass |
|---|---|---|
| Manual remote-host pairing and connection **without an AO Cloud account** (fork #14). The `remotes:list/add/update/remove/connect/disconnect` IPC handlers must not call `requireAccount` or need an account id. Only `remotes:syncAccountHosts` and `remotes:linkAccountHost` require one. Manual pairing codes exchange directly with the host for an encrypted per-device token, which survives desktop restart. | `frontend/src/main/remotes-main.ts`, `remotes-ipc.ts`, `remotes-store.ts`, `frontend/src/renderer/components/settings/RemoteHostsSettings.tsx`, `components/RemoteHostsSection.tsx`, `hooks/useRemoteHosts.ts`, `lib/account-remote-hosts.ts`, `routes/_shell.tsx`, `docs/self-hosted-remote.md` | `remotes-main.test.ts`, `remotes-ipc.test.ts`, `remotes-store.test.ts`, `RemoteHostsSettings.test.tsx`, `RemoteHostsSection.test.tsx`, `useRemoteHosts.test.tsx`, `useRemoteHosts.self.test.tsx` |
| Local credentials stay local. `~/.ao/remotes.json` entries without `accountUserId` are manual pairings: they are never uploaded, never issued account tokens and never pruned by cloud import. | `frontend/src/main/remotes-main.ts`, `remotes-store.ts` | as above |
| Desktop remote proxy, including the HTTP/2 destroyed-session fix (fork #17). | `frontend/src/main/remote-proxy.ts` | `remote-proxy.test.ts` |
| LAN listener: `ao remote-host enable/disable/status`, port 3011, LAN-only mode without a public tunnel. | `backend/internal/httpd/lan_listener.go`, `backend/internal/httpd/controllers/mobile*.go`, `backend/internal/cli/remote_host.go`, `backend/internal/daemon/remote_host_directory.go`, `backend/internal/mobilebridge/` (`DefaultPort = 3011`) | `go test` on `./internal/httpd` (`TestLAN*`, `TestMobileLAN*`; in `lan_*_test.go`, `auth_test.go`, `mobile_connect_telemetry_test.go`), `./internal/httpd/controllers` (`TestLANOnly*`, `TestTunnelOnly*`, `TestMobileStatus*`), `./internal/cli` (`TestRemoteHost*`), `./internal/daemon` (`TestPublishRemoteHost*`), `./internal/mobilebridge` |
| Sign-in-free use, plus ssh and CLI control of a host: the daemon and `ao` CLI work with no account. | whole app; watch new `requireAccount`/`requireSignIn` gates | guard: gating sections |
| Telemetry defaults OFF on fresh and upgraded installs. Usage analytics requires the explicit `telemetry_opt_in` marker; error reporting requires a version-3 policy written by a user choice. Missing, unsafe or unreadable consent fails closed. The `telemetry_opt_out` marker always wins. Tests include fresh packaged startup with no PostHog import, no renderer fetch and no Electron error transport, and daemon zero-request regressions. | `frontend/src/shared/telemetry-consent.ts`, `telemetry-policy.ts`, `frontend/src/main/telemetry-policy-file.ts`, `backend/internal/adapters/telemetry/consent.go`, `policyauthority/reader.go` | `fork-telemetry-defaults.test.ts`, policy/controller tests, `TestFork*`, policy-authority tests |
| Telemetry opt-out: the `telemetry_opt_out` marker file next to the install id must keep stopping export: daemon and renderer PostHog, usage/session telemetry, and daemon and Electron Sentry, whatever `telemetry_policy.json` says. | `backend/internal/adapters/telemetry/optout.go`, `identity.go`, `backend/internal/daemon/fork_sentry_optout.go`, `frontend/src/main/telemetry-identity.ts`, `frontend/src/main/sentry-main.ts` | `TestOptOut*` in `./internal/adapters/telemetry` and `./internal/daemon`, `telemetry-identity.test.ts`, `sentry-main.test.ts` |
| Sessions and worktrees are kept by default. Upstream #7440 added a daemon pass that deletes archived sessions after `session_retention_days`; the default must stay 0 (keep), and the cleanup reconcile must never remove a worktree the user did not accept for cleanup. | `backend/internal/storage/sqlite/migrations/20261030182857237_session_retention.sql`, `backend/internal/service/session/service.go` (`DeleteExpired`), `backend/internal/session_manager/workspace_cleanup.go` | `TestForkSessionRetentionDefaultsToKeep` in `./internal/storage/sqlite/store`, `TestDeleteExpired*` in `./internal/service/session` |
| Sentry DSNs stay empty in dev builds. Production defaults may only be used when `isDev` is false, and `AO_SENTRY_DSN=""` / `AO_ELECTRON_SENTRY_DSN=""` must keep disabling them. | `frontend/src/shared/sentry-config.ts`, `frontend/src/main/sentry-main.ts`, `frontend/src/main.ts`, `backend/internal/config/config.go`, `backend/internal/observe/sentryobs/` | `sentry-config.test.ts`, `sentry-main.test.ts` |

The upstream desktop minimum-version check must not run on fork startup. Its
remote policy can otherwise force a local app to quit. `fork-version-floor.test.ts`
protects the omission and the independent updater startup; the latter still reads
the fork release feed from `app-update.yml`.

The Usage page (Spend and Limits, raw token counts by default) and the Cmd+K
palette are normal features of the local-distribution build, not Developer Mode
or nightly ones. Developer Mode keeps its default of off and still gates the
experimental Windows tab; Teams stays off. Watch `routes/_shell.usage.tsx`,
the Usage rows in `Sidebar.tsx` and `lib/command-palette.ts`, and
`lib/build-channel.ts`; `fork-local-usage.test.tsx` protects them.

Artifact Publish is also protected: desktop `cloudShare:publish` and Cloud requests must refuse signed-out calls before uploading or contacting the control plane. Manual credentials cannot be linked through the desktop Cloud API; link a machine explicitly from its own CLI if desired.

Fork guard and Public boundary are unconditional reusable jobs of CI on every PR and push to main. `ci-ok` requires both to succeed, even when other lanes are skipped.

All of these run through `scripts/fork/protected-tests.sh`, which holds the
file list and a minimum test count per file or `go test` selection.

The guard also flags, for review: new outbound `https://`/`wss://` endpoints,
telemetry/Sentry/PostHog defaults (DSNs, API keys), new account or cloud
gating, new SQLite migrations, and package manifest or lockfile changes.

## Fork-only changes to preserve

Check that each of these survives every sync. The guard lists the files when
upstream touches them.

| Fork PR | Change | Files |
|---|---|---|
| #7 | Setup finishes before synchronous spawn times out. **Superseded** by upstream; no longer needs preserving. | n/a |
| #5, #8 | Repeated chat Stop keeps cancellation | `backend/internal/adapters/chatdriver/acp/conversation.go`, `backend/internal/service/chat/controller.go` |
| #2 | Kimi ACP permission-mode mapping | `backend/internal/adapters/chatdriver/kimiacp/driver.go`, `backend/internal/session_manager/chat_spawn.go` |
| #3, #4 | Kimi instructions path for Kimi Code 2.x | `backend/internal/adapters/agent/kimi/` |
| #14 | Manual remote hosts without AO Cloud sign-in | see Protected capabilities |
| #15, #16 | sqlc output regenerated; fork automation queries select explicit columns | `backend/internal/storage/sqlite/queries/automations.sql`, `backend/internal/storage/sqlite/gen/automations.sql.go` |
| #17 | Ignore connect on an upstream HTTP/2 session already destroyed | `frontend/src/main/remote-proxy.ts` |
| #18 | React 19.3.1 (dev `performance.measure` leak), Electron 44.10.0, DOMPurify 3.4.17 | `frontend/package.json`, `packages/product-ui/package.json` and lockfiles. Never take a lower version. |
| #1 | Reload the shell when its renderer process dies | `frontend/src/main/shell-crash-recovery.ts` |
| #6 | Recurring automations unblock after completed Chat tasks | `backend/internal/service/automation/scheduler.go`, `store/automation_store.go` |
| #10 | ACP install isolated from inherited npm script policy | `frontend/scripts/build-acp-runtime*.mjs` |
| #11 | DeepSeek ACP honors bypass permissions | `backend/internal/adapters/chatdriver/deepseekharnessacp/driver.go` |
| sync 2026-11-01 | The `telemetry_opt_out` marker also stops daemon and Electron Sentry (upstream #7437 turned Electron Sentry on for consented users) | `backend/internal/daemon/fork_sentry_optout.go`, `daemon.go` (`SetPolicyCheck`), `frontend/src/main/sentry-main.ts` |
| sync 2026-11-06 | The marker also stops upstream #7682 daemon background reports (`CaptureBackground`) and Electron handled main errors (`captureMain`); manual remote-host IPC stays ungated after upstream switched its errors to `ExpectedIpcError` | `frontend/src/main/sentry-main.ts`, `frontend/src/main/remotes-main.ts`, `backend/internal/daemon/fork_sentry_optout_test.go` |

## Build versions (Icosa builds)

**Do not bump versions by PR; build-icosa stamps them.** `frontend/package.json`
holds only the base convention `<base>-icosa.<floor>` (currently
`0.14.8-icosa.2`, the floor of builds made before stamping). Every
`frontend/scripts/build-icosa-local.sh` run (via `ao-build` / `build-icosa`)
stamps `<base>-icosa.<N>`:

- `ICOSA_VERSION=<base>-icosa.<N>` pins a version; it must match the base in
  `package.json` and N must be above the floor (pre-stamping builds such as
  `0.14.8-icosa.1`/`.2` may live only on aofiles, which the build agent cannot
  read).
- Otherwise N is 1 + the highest N already built for that base in the publish
  dir, the legacy `/Volumes/aofiles/ao-builds/icosa-orchestrator` (read-only,
  only when readable) and `frontend/out/local`, and never at or below the floor.
  Auto-numbering refuses to run if the publish dir cannot be listed.
- Builds publish to `ICOSA_PUBLISH_DIR`, default
  `~/ao-builds/icosa-orchestrator/<version>/` on the Studio's internal disk (the
  launchd build agent has no TCC access to the external aofiles disk). The
  fleet updater fetches from there. A version present in either place is never
  overwritten.
- An upstream sync that changes the base (e.g. to `0.14.9`) starts again at
  `0.14.9-icosa.1`; the sync PR resolves the `package.json` conflict to the new
  upstream version (with or without an `-icosa.0` suffix).

The stamp goes into `package.json`/`package-lock.json` for the build only
(restored on exit, never committed), so `CFBundleShortVersionString`,
`app.getVersion()`, `ao version` (ldflags), artifact names and `manifest.json`
all carry it. `CFBundleVersion` is numeric and monotonic:
`<major>.<minor>.<patch*1000+N>` (N <= 999). Existing versions are never
overwritten. The designated requirement must not contain the version and must
match the previous build's (TCC retention); `verification.log` records the
check and the build fails on a change unless `ICOSA_ALLOW_DR_CHANGE=1`.
`verify-mac-artifact.sh` ships next to the artifacts, with `verifierSha256` in
`manifest.json`. `frontend/scripts/icosa-version.mjs` holds the version logic;
`build-icosa-local.node-test.mjs` tests it.

## Fleet seams (fleet messaging and control, PR-0)

Fleet messaging and control lives in fork-owned code. It attaches to
upstream-owned files only through these small seams, each marked with a
`Fork: fleet seam (FORK.md)` comment. Each is inert while unset, so with the
default wiring (`backend/internal/daemon/fork_fleet_wiring.go`, all no-ops in
PR-0) the daemon behaves exactly as upstream.

| Seam | Upstream file | What plugs in | Default |
|---|---|---|---|
| `APIDeps.Extensions []func(chi.Router)`, mounted after `api.Register` | `backend/internal/httpd/api.go`, `router.go` | fork `/api/v1/fleet/v1/*` routes | nil: nothing mounted |
| `BridgeService.OnDevicePaired(code, device) error`, called after the device is stored and before its token is returned; an error revokes the device and fails pairing, so no token leaves | `backend/internal/httpd/controllers/mobile.go` | binds a fleet grant to the paired device | nil: pairing unchanged |
| LAN handler wrap: `NewMobileLAN(fleet.lanHandler(srv.Handler()), ...)`; the wrap runs inside the LAN control block and `authMiddleware`, so it only narrows access | `backend/internal/daemon/daemon.go` | fleet scope gate | identity |
| `CoordinatorDeps.Router` (`report.Router`); a handled batch is acknowledged with no Submit, Interrupt or local wake, and never piggybacked | `backend/internal/service/report/coordinator.go` (+ `coordinator_router.go`) | master-only report hand-off (Q4) | nil: local delivery unchanged |
| `markRemoteFleetWrite(cmd)` on `spawn`, `send`, `session kill` | `backend/internal/cli/spawn.go`, `send.go`, `session.go` | fleet write client (`--host`) | still refused like any remote write |

Tests: `TestForkFleet*`, `TestForkOnDevicePaired*`, `TestForkDeviceToken*`
(`./internal/httpd`, including the issuance-path invariant that only
`BridgeService.PairDevice` mints device tokens), `TestForkReportRouter*` with
all `TestCoordinator*` (`./internal/service/report`), `TestForkFleetWiring*`
(`./internal/daemon`) and `TestForkFleetWrite*` (`./internal/cli`), all in
`protected-tests.sh`. The guard flags any `Fork: fleet seam` marker lost
between the merge base and HEAD; `scripts/fork/upstream-guard.test.sh`
checks that it does.

## Usage seams (API-billed usage, PR A: DeepSeek price book; PR B: report extension)

The DeepSeek price book lives in fork-owned code:
`backend/internal/forkusage/pricebook` (embedded price versions with evidence,
the 2026 and 2027 Chinese public holiday calendars from the State Council
notices, the peak/off-peak window engine, the read-time pricer and the
automatic price-page check), `backend/internal/daemon/fork_usage_wiring.go`
and `backend/internal/cli/fork_usage_prices.go` (`ao usage prices`,
`ao usage prices --check`, `--accept`, `ao usage prices auto on|off`,
`ao usage window`). It attaches to upstream-owned files only through these
seams, each marked with a `Fork: usage seam (FORK.md)` comment; the guard
counts them together with the fleet seam markers.

| Seam | Upstream file | What plugs in |
|---|---|---|
| `startForkUsage` / `defer forkUsage.stop()` next to `newForkFleet()` (given the store), and `forkUsage.routes()` appended to `APIDeps.Extensions` | `backend/internal/daemon/daemon.go` | the price book, its daily check loop and the `/api/v1/usage/fork/*` routes, including `report-ext` |
| `requestStartedAt` capture: the last `request/context` frame before each reply, written into the event's `provider_usage_json` | `backend/internal/observe/usage/parser_deepseek.go` | the request start the report extension uses for requests spanning a peak/off-peak boundary (no schema change) |
| `--by hour`, `--json`, the help text, and one merge call each after the single-host and fleet report reads | `backend/internal/cli/usage.go` | `backend/internal/cli/fork_usage_report.go`: the `ext` merge, `--top` re-ranking by price-book cost, the hour histogram |
| the report test server answers `/api/v1/usage/fork/*` with 404 and the exact-output test drops `ext` | `backend/internal/cli/usage_test.go` | keeps the upstream report tests meaningful with the extra report-ext request |
| `prices` and `window` added to the `ao usage` tree | `backend/internal/cli/usage.go` | fork CLI commands; `prices` writes and stays local, `window` is remote read-only |
| `ExitCode` honours an error's own `ExitCode() int` | `backend/internal/cli/root.go` | `ao usage prices --check` exit codes 3 (changed) and 4 (cannot verify) |
| `ao usage prices`, `prices auto`, `window` listed as user commands | `backend/internal/telemetrymeta/cli.go` | CLI command classification (no telemetry is sent unless consented) |

The routes sit under `/api/v1/usage/fork/` (not `/api/v1/fork/usage/` as the
design sketched) so the LAN listener's device-credential gate for
`/api/v1/usage` covers them. Prices are computed at read time; nothing is
stored in SQLite and no upstream pricing, DTO, OpenAPI or migration changes.

PR B adds `backend/internal/forkusage/reportext` (read-time DeepSeek report
extension: per-event window basis, pricing by price version, route and
billing status, `boundaryTokens`, per-session models, the UTC hour-of-day
histogram; route `GET /api/v1/usage/fork/report-ext`) and the fork-owned
read-only query `backend/internal/storage/sqlite/store/fork_usage_deepseek_events.go`
(the upstream report joins plus `requestStartedAt`;
`TestForkUsageDeepSeekEvents*` fails if upstream renames a column it reads).
The merge contract, the window rule and the `--by hour` JSON are documented
in [`docs/provider-usage.md`](docs/provider-usage.md).

State, all under `$AO_DATA_DIR/provider-usage/` (files 0600):
`config.json` (`pricebook.autoCheck`, default true; unknown keys are kept for
later provider settings), `pricebook/deepseek-auto.json` (versions recorded
from the page, append-only) and `pricebook/check-state.json`.

Price-page check rules: at most once per 24 h (first run 5-30 min after
start), keyless, no cookies, redirects refused, HTTPS only, body at most
512 KB, 10 s timeout. A parsed change is recorded as a new version labelled
"auto-updated from page", effective from the fetch instant; earlier versions
are never edited. A price moving more than 10x either way, a currency other
than `$`, or more than half the models vanishing is held as suspicious until
`ao usage prices --accept <pageSha256>`. A failed fetch or unrecognized page
keeps the last version and warns; prices turn stale 7 days after the last good
check (30 days with the check off).

### Default outbound endpoints

Requests the daemon sends by default, with no opt-in. Every new default
destination must be added here and to `TestForkDefaultOutboundHosts`
(`./internal/daemon`), which runs the price fetchers with default settings
against a recording transport and fails on any other host, a non-HTTPS
request, or a request carrying an `Authorization` header or cookies. The
packaged signed-out network check was a manual procedure (see the release
verification reports); add these hosts to its expected destinations.

| Host and path | Purpose | Schedule | Switch | Credentials |
|---|---|---|---|---|
| `raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json` | LiteLLM model price table (upstream usage cost estimates) | daily refresh | upstream behaviour | none |
| `api-docs.deepseek.com/quick_start/pricing.md` | DeepSeek price-page check (price book drift) | at most daily, first 5-30 min after start; also `ao usage prices --check` | `ao usage prices auto off` (`TestForkPriceCheckSwitchOff`) | none, no cookies |

Tests: `./internal/forkusage/pricebook` (all), `TestForkDefaultOutboundHosts`
and `TestForkPriceCheckSwitchOff` (`./internal/daemon`), `TestForkUsageRoutes*`
(`./internal/httpd`), `TestForkUsage*` (`./internal/cli`),
`./internal/forkusage/reportext` (all, including the golden 30-day sample:
$21.67 windowed vs $36.03 peak-only), `TestForkUsageSeam*`
(`./internal/observe/usage`), `TestForkUsageDeepSeekEvents*`
(`./internal/storage/sqlite/store`) and `TestForkUsageWiring*`
(`./internal/daemon`), all in `protected-tests.sh`.

## Upstream-sync review procedure (mandatory)

Never use GitHub's "Sync fork" button, `gh repo sync` or the merge-upstream
API. Every sync goes through a `sync/upstream-YYYY-MM-DD` PR, so the
`Fork guard` workflow reviews it and runs the protected tests.

```sh
git fetch origin && git fetch upstream

# 1. Review what upstream would bring in. Exit 0 = clean, 2 = needs review.
scripts/fork/upstream-guard.sh origin/main..upstream/main | tee /tmp/upstream-guard.md

# 2. Read every flagged section and the commits behind it.
git log --oneline origin/main..upstream/main -- <flagged path>
git show <commit> -- <flagged path>

# 3. Merge on a sync branch (merge, never rebase or force-push).
git switch -c sync/upstream-$(date +%F) origin/main
git merge --no-ff upstream/main

# 4. Re-run the guard on the merge result and run the protected tests. The test
#    script fails when a protected test fails, or when a listed test file or go
#    test selection disappears or drops below its floor.
scripts/fork/upstream-guard.sh origin/main..HEAD
(cd frontend && npm ci --ignore-scripts)
scripts/fork/protected-tests.sh

# 5. Open the sync PR against the fork only, with the guard report in the body.
git push origin HEAD
gh pr create -R Icosa2050/agent-orchestrator --base main --title "Sync upstream main to $(git rev-parse --short upstream/main)"
```

What to check in each flagged section of the report:

- **DELETED protected files:** a blocker unless the PR keeps an equivalent
  local path and test.
- **Fork fleet seam markers removed:** a blocker. Restore the seam (see Fleet
  seams) when resolving the merge; the protected seam tests must pass.
- **Account / sign-in gating:** any new gate on add, connect, list, the LAN
  listener or the CLI is a conflict (see below).
- **Outbound endpoints, DSN / API-key defaults:** a new endpoint must require explicit opt-in; an absent opt-out is not consent. Dev builds must keep empty DSNs.
- **Telemetry opt-out / dev DSN guard changed:** the `telemetry_opt_out` marker
  must still stop export, and an opted-out install must stay opted out after
  the sync.
- **Migrations:** check that they do not move local data or credentials into
  cloud-only tables, and that they apply on top of the fork's schema.
- **Packages:** check for new telemetry or cloud SDKs, and keep the fork's
  versions from #18 or newer.
- **CI workflows / AGENTS.md:** upstream cloud-contract or boundary guards must
  not start failing local-only fork changes.

## Resolving a conflicting upstream change

1. Stop the sync. Note the offending commits in the sync PR description and
   ask the owner.
2. In the sync PR, keep the upstream change wherever possible, but restore the
   local capability: for example, keep the cloud path while leaving manual or
   signed-out paths ungated, or put a new endpoint behind the opt-out. Add or
   keep a regression test in the protected test set.
3. The owner reviews and merges. Merge with a merge commit; do not rebase or
   force-push `main`.
4. If upstream removes a capability outright (code moved to the private
   control plane), keep the fork's implementation and record it as a fork-only
   change in the table above.

## Sync 2026-11-16 additions

- `ao triage`, the bundled bug-triage skill, duplicate search, posting URL
  validation and Report a problem links target `Icosa2050/agent-orchestrator`.
  Terminal posting retains the default-no `[y/N]` confirmation; in-app agents
  must preview the full payload and obtain an explicit yes. `TestForkTriageTargetsFork`
  protects the destination and rejects upstream issue URLs.
- Agent-account routes at `/api/v1/agents/accounts` (including nested paths)
  require a paired per-device token or the account token on the authenticated
  LAN listener. The shared pairing password is always refused for this area,
  independently of `require-device-credentials`. The primary loopback listener
  remains unchanged. `TestForkAgentAccounts*` covers password refusal, paired
  device acceptance, revocation, account tokens, loopback and strict origins.
- Session deep-link readiness follows manual host-list completion even while
  signed out; signing out must not imply that hand-paired hosts cannot exist.
- Upstream #8492's rotated Sentry DSNs remain behind the existing explicit
  consent and legacy opt-out veto. Daemon tests use the rotated production DSN
  with an injected recording transport; Electron tests use the current default.

## Desktop host-profile platforms

`~/.ao/icosa-desktop.json` is supported on macOS and Linux. Electron and the Go
CLI validate its owner, owner-only POSIX permissions, schema and paths before
using it; malformed or unsafe profiles fail closed even with environment overrides.
Windows ignores this file entirely, including malformed or unreadable files:
POSIX mode/uid checks cannot establish Windows ACL safety. No Windows profile
support is claimed until an ACL-based contract is implemented in both readers.
On Windows, explicit `AO_DATA_DIR`, `AO_RUN_FILE` and `AO_PORT` still win over the
approved defaults (`~/.ao/dev/data`, `~/.ao/dev/running.json`, port 3002).
