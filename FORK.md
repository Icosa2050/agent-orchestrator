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
| Manual remote-host pairing and connection **without an AO Cloud account** (fork #14). The `remotes:list/add/update/remove/connect/disconnect` IPC handlers must not call `requireAccount` or need an account id. Only `remotes:importAccountHost`, `remotes:pruneAccountHosts` and `remotes:issueAccountToken` may require one. | `frontend/src/main/remotes-main.ts`, `remotes-ipc.ts`, `remotes-store.ts`, `frontend/src/renderer/components/settings/RemoteHostsSettings.tsx`, `components/RemoteHostsSection.tsx`, `hooks/useRemoteHosts.ts`, `lib/account-remote-hosts.ts`, `routes/_shell.tsx`, `docs/self-hosted-remote.md` | `remotes-main.test.ts`, `remotes-ipc.test.ts`, `remotes-store.test.ts`, `RemoteHostsSettings.test.tsx`, `RemoteHostsSection.test.tsx`, `useRemoteHosts.test.tsx`, `useRemoteHosts.self.test.tsx` |
| Local credentials stay local. `~/.ao/remotes.json` entries without `accountUserId` are manual pairings: they are never uploaded, never issued account tokens and never pruned by cloud import. | `frontend/src/main/remotes-main.ts`, `remotes-store.ts` | as above |
| Desktop remote proxy, including the HTTP/2 destroyed-session fix (fork #17). | `frontend/src/main/remote-proxy.ts` | `remote-proxy.test.ts` |
| LAN listener: `ao remote-host enable/disable/status`, port 3011, LAN-only mode without a public tunnel. | `backend/internal/httpd/lan_listener.go`, `backend/internal/httpd/controllers/mobile*.go`, `backend/internal/cli/remote_host.go`, `backend/internal/daemon/remote_host_directory.go`, `backend/internal/mobilebridge/` (`DefaultPort = 3011`) | `go test` on `./internal/httpd` (`TestLAN*`, `TestMobileLAN*`; in `lan_*_test.go`, `auth_test.go`, `mobile_connect_telemetry_test.go`), `./internal/httpd/controllers` (`TestLANOnly*`, `TestTunnelOnly*`, `TestMobileStatus*`), `./internal/cli` (`TestRemoteHost*`), `./internal/daemon` (`TestPublishRemoteHost*`), `./internal/mobilebridge` |
| Sign-in-free use, plus ssh and CLI control of a host: the daemon and `ao` CLI work with no account. | whole app; watch new `requireAccount`/`requireSignIn` gates | guard: gating sections |
| Telemetry opt-out: the `telemetry_opt_out` marker file next to the install id must keep stopping export. | `backend/internal/adapters/telemetry/optout.go`, `identity.go`, `frontend/src/main/telemetry-identity.ts` | `TestOptOut*` in `./internal/adapters/telemetry`, `telemetry-identity.test.ts` |
| Sentry DSNs stay empty in dev builds. Production defaults may only be used when `isDev` is false, and `AO_SENTRY_DSN=""` / `AO_ELECTRON_SENTRY_DSN=""` must keep disabling them. | `frontend/src/shared/sentry-config.ts`, `frontend/src/main/sentry-main.ts`, `frontend/src/main.ts`, `backend/internal/config/config.go`, `backend/internal/observe/sentryobs/` | `sentry-config.test.ts`, `sentry-main.test.ts` |

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
- **Account / sign-in gating:** any new gate on add, connect, list, the LAN
  listener or the CLI is a conflict (see below).
- **Outbound endpoints, DSN / API-key defaults:** a new endpoint must be opt-in
  or covered by the opt-out. Dev builds must keep empty DSNs.
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
