# Manual Remote Hosts Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Restore manual remote hosts without AO Cloud sign-in and open a tested draft PR.
**Architecture:** Local pairings have no account owner. Account-owned pairings retain their owner and require that account. Renderer controls and IPC enforce the same distinction; cloud discovery remains optional via account sign-in.
**Tech Stack:** Electron, TypeScript, React, Vitest.
**Spec:** ../specs/2026-10-28-manual-remotes-design.md

## Global Constraints

- No running AO restart, no live credential edits, no backend listener/auth changes.
- Preserve identity, self-connection, file-permission, and account-isolation checks.
- Do not upload manually supplied credentials or automatically issue account tokens.

## Review Focus

- Signed-out preview/browser routes must not reach stale account-owned proxies.
- Account import must not overwrite local credentials or another account's records by URL/host collision.
- Hexadecimal manual passwords must survive cloud pruning.
- Account switching during asynchronous probes must not present credentials in a changed context.
- Cloud failure must not hide or disable local pairings.

## Task 1: Restore the local IPC path

- [x] Add signed-out CRUD/connect/preview tests and cloud ownership/import/prune regressions; watch them fail.
- [x] Split optional account context from strictly authenticated cloud operations; retain checks and local-only manual records.
- [x] Run the complete remote main-process test group.

## Task 2: Restore settings and connection behavior

- [x] Add signed-out settings/hook and no-upload tests; watch them fail.
- [x] Always show manual pairing, explain optional discovery, and keep cloud sync out of manual CRUD.
- [x] Connect local hosts while signed out; retain account-transition cancellation and optional signed-in discovery.
- [x] Update documentation and translations for the changed explanation.
- [x] Run frontend typecheck and the full frontend suite, record baseline/environment failures separately.

## Task 3: Verify and publish

- [x] Capture before/after settings from an isolated app with scratch data.
- [x] Review changes, fix supported findings, and verify affected tests.
- [ ] Commit, push, open draft PR with counts, evidence, and validation gaps, then inspect CI and attach the PR.

## Verification evidence

- Focused remote, settings, hook, and route tests: 266 passed across 10 files.
- Full frontend suite: 9,751 passed, 6 skipped across 580 files; project and E2E typechecks passed.
- Native Electron signed-out before/after captured with the same isolated scratch profile.
- Independent review identified an asynchronous account-change persistence race and stale account rows in settings; regression tests reproduced both before fixes and now pass.
- SQLite storage tests passed. Backend build and vet passed; two race-suite failures also reproduce on unchanged base (CommandCode conditional git include and Crush local authentication).
- Regenerated sqlc with repository-pinned v1.31.1 to repair existing main-branch generated-query drift requested by the owner.
