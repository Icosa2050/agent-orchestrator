import { describe, expect, it } from "vitest";
import type { AgentAccount } from "../hooks/useAgentAccounts";
import { buildFleetLimitRows, classifyWindow, hostStateFromError, type FleetHostInput } from "./fleet-limits";

const now = new Date("2026-11-17T14:00:00Z");
const ago = (minutes: number) => new Date(now.getTime() - minutes * 60_000).toISOString();
const ahead = (hours: number) => new Date(now.getTime() + hours * 3_600_000).toISOString();

function account(overrides: Partial<AgentAccount>): AgentAccount {
	return { id: "a", provider: "claude", default: false, device: false, loggedIn: true, signedIn: true, ...overrides };
}

const ok = { kind: "ok" } as const;

describe("buildFleetLimitRows", () => {
	it("merges an email across hosts and shows the freshest reading per window", () => {
		const rows = buildFleetLimitRows([
			{ hostId: "a", label: "A", state: ok, accounts: [account({ id: "device-claude", device: true, email: "Me@Example.com", plan: "max", limits: { fetchedAt: ago(2), windows: [{ id: "seven_day", label: "Weekly", usedPercent: 30, resetsAt: ahead(48), durationMinutes: 10080 }] } })] },
			{ hostId: "b", label: "B", state: ok, accounts: [account({ id: "acct-1", email: "me@example.com", limits: { fetchedAt: ago(50), windows: [{ id: "seven_day", label: "Weekly", usedPercent: 70, resetsAt: ahead(48), durationMinutes: 10080 }, { id: "five_hour", label: "Session (5h)", usedPercent: 90, resetsAt: ago(10), durationMinutes: 300 }] } })] },
		], now);
		expect(rows).toHaveLength(2);
		const [fiveHour, weekly] = rows;
		expect(weekly).toMatchObject({ email: "Me@Example.com", plan: "max", usedPercent: 30, readAt: ago(2), readOn: { hostId: "a" }, window: { kind: "weekly" } });
		expect(weekly.hosts.map((host) => host.hostId)).toEqual(["a", "b"]);
		// The 5h window already reset: its old level is not shown.
		expect(fiveHour).toMatchObject({ window: { kind: "5h" }, usedPercent: null, resetPassed: true, readOn: { hostId: "b" } });
	});

	it("takes a newer limit-history reading for an account the host lists", () => {
		const rows = buildFleetLimitRows([{
			hostId: "local", state: ok,
			accounts: [account({ id: "device-codex", provider: "codex", email: "me@example.com", limits: { fetchedAt: ago(10), windows: [{ id: "codex:Weekly", label: "Weekly", usedPercent: 40, resetsAt: ahead(30) }] } })],
			logins: [
				{ accountId: "device-codex", provider: "codex", email: "me@example.com", windows: [{ windowId: "codex:Weekly", label: "Weekly", firstReadAt: ago(600), lastReadAt: ago(1), peakUsedPercent: 42, readings: 9, resetsAt: ahead(30), state: "open", points: [{ at: ago(1), usedPercent: 42 }] }] },
				// A login the host no longer lists adds nothing.
				{ accountId: "gone", provider: "codex", email: "old@example.com", windows: [{ windowId: "codex:Weekly", label: "Weekly", firstReadAt: ago(600), lastReadAt: ago(1), peakUsedPercent: 5, readings: 1, resetsAt: ahead(30), state: "open" }] },
			],
		}], now);
		expect(rows).toHaveLength(1);
		expect(rows[0]).toMatchObject({ usedPercent: 42, readAt: ago(1) });
	});

	it("keeps accounts without an email per host and splits an email across two known orgs", () => {
		const hosts: FleetHostInput[] = [
			{ hostId: "local", state: ok, accounts: [account({ id: "device-claude", device: true, signedIn: false, loggedIn: false }), account({ id: "x", email: "me@example.com", orgId: "org-1" })] },
			{ hostId: "a", label: "A", state: ok, accounts: [account({ id: "device-claude", device: true, signedIn: false, loggedIn: false }), account({ id: "y", email: "me@example.com", orgId: "org-2" })] },
		];
		const rows = buildFleetLimitRows(hosts, now);
		expect(rows.filter((row) => row.email === "me@example.com")).toHaveLength(2);
		const anonymous = rows.filter((row) => row.email === null);
		expect(anonymous.map((row) => row.hosts.map((host) => host.hostId))).toEqual([["a"], ["local"]]);
		expect(anonymous.every((row) => row.window === null)).toBe(true);
	});
});

describe("classifyWindow", () => {
	it.each([
		[{ id: "five_hour", label: "Session (5h)", durationMinutes: 300 }, "5h", null],
		[{ id: "seven_day", label: "Weekly", durationMinutes: 10080 }, "weekly", null],
		[{ id: "model:Fable", label: "Weekly · Fable", durationMinutes: 10080 }, "weekly-model", "Fable"],
		[{ id: "codex:Weekly", label: "Weekly", durationMinutes: 10080 }, "weekly", null],
		[{ id: "gpt5:GPT-5 · Weekly", label: "GPT-5 · Weekly", durationMinutes: 10080 }, "weekly-model", "GPT-5"],
		[{ id: "codex:Limit", label: "Limit" }, "other", null],
	])("classifies %o", (window, kind, model) => {
		expect(classifyWindow(window)).toEqual({ kind, model });
	});
});

describe("hostStateFromError", () => {
	it("names what a person must do", () => {
		expect(hostStateFromError({ status: 401, code: "DEVICE_CREDENTIAL_REQUIRED" }, false, 0)).toEqual({ kind: "repair" });
		expect(hostStateFromError({ status: 401, code: "DEVICE_REVOKED" }, true, 0)).toEqual({ kind: "repair" });
		expect(hostStateFromError({ status: 404 }, false, 0)).toEqual({ kind: "update" });
		expect(hostStateFromError({ status: 502, code: "UPSTREAM_UNAVAILABLE" }, false, 0)).toEqual({ kind: "offline" });
		expect(hostStateFromError({ status: 502, code: "UPSTREAM_UNAVAILABLE" }, true, now.getTime())).toEqual({ kind: "stale", since: now.toISOString() });
		expect(hostStateFromError(Object.assign(new Error("boom"), { status: 500 }), false, 0)).toEqual({ kind: "error", message: "boom" });
	});
});
