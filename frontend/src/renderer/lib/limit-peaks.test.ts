import { describe, expect, it } from "vitest";
import { buildPeakHistoryRows, summarizePeaks, type UsageLimitPeak, type UsageLimitPeakTrack } from "./limit-peaks";

const day = (n: number) => new Date(Date.UTC(2026, 10, n, 8)).toISOString();
const period = (reset: number, peak: number, readings = 100, extra: Partial<UsageLimitPeak> = {}): UsageLimitPeak => ({
	resetAt: day(reset), peakUsedPct: peak, readings, firstReadingAt: day(reset - 7), lastReadingAt: day(reset - 0.01), open: false, ...extra,
});
const track = (email: string | undefined, periods: UsageLimitPeak[], extra: Partial<UsageLimitPeakTrack> = {}): UsageLimitPeakTrack => ({
	provider: "claude", login: email?.toLowerCase() ?? "account:device-claude", email, accountId: "device-claude", plan: "max",
	windowId: "seven_day", label: "Weekly", durationMinutes: 10080, periods, days: [], summary: summarizePeaks(periods), ...extra,
});

describe("buildPeakHistoryRows", () => {
	it("merges one login's periods across hosts: the highest peak and the freshest reset win", () => {
		const rows = buildPeakHistoryRows([
			{ hostId: "local", windows: [track("Me@example.com", [period(5, 40), period(12, 91, 50)])] },
			{ hostId: "host-a", label: "MacStudio", windows: [track("me@example.com", [
				// The same reset, rounded differently.
				period(12, 70, 30, { resetAt: new Date(Date.parse(day(12)) + 60_000).toISOString() }),
				// The provider moved this reset on this host only; the readings overlap the local period.
				period(20, 25, 10, { resetAt: day(21), firstReadingAt: day(13), lastReadingAt: day(18) }),
			])] },
			{ hostId: "host-b", windows: [track("me@example.com", [period(19, 20, 5, { firstReadingAt: day(12.5), lastReadingAt: day(19.5), open: true })])] },
		]);
		expect(rows).toHaveLength(1);
		const [row] = rows;
		expect(row.hosts.map((host) => host.hostId)).toEqual(["local", "host-a", "host-b"]);
		expect(row.periods.map((p) => [p.peakUsedPct, p.readings])).toEqual([[40, 100], [91, 80], [25, 15]]);
		// Host B read last, so its reset and open state name the merged period.
		expect(row.periods[2].resetAt).toBe(day(19));
		expect(row.periods[2].open).toBe(true);
		expect(row.summary).toEqual({ periods: 2, avgPeak: 65.5, maxPeak: 91, periodsOver80: 1, periodsUnder30: 0 });
	});

	it("keeps logins without an email apart per host and orders codex first, 5h before weekly", () => {
		const rows = buildPeakHistoryRows([
			{ hostId: "local", windows: [track(undefined, [period(5, 10)])] },
			{ hostId: "host-a", windows: [
				track(undefined, [period(5, 90)]),
				track("b@example.com", [period(5, 10)], { provider: "codex", windowId: "codex:weekly", label: "Weekly" }),
				track("b@example.com", [period(5, 10)], { provider: "codex", windowId: "codex:5h", label: "5h", durationMinutes: 300 }),
			] },
		]);
		expect(rows.map((row) => [row.provider, row.email, row.window.kind])).toEqual([
			["codex", "b@example.com", "5h"], ["codex", "b@example.com", "weekly"], ["claude", null, "weekly"], ["claude", null, "weekly"],
		]);
	});
});

describe("summarizePeaks", () => {
	it("leaves the open period out and counts the bands", () => {
		expect(summarizePeaks([period(1, 85), period(2, 29.9), period(3, 80), period(4, 99, 1, { open: true })]))
			.toEqual({ periods: 3, avgPeak: (85 + 29.9 + 80) / 3, maxPeak: 85, periodsOver80: 2, periodsUnder30: 1 });
		expect(summarizePeaks([])).toEqual({ periods: 0, avgPeak: 0, maxPeak: 0, periodsOver80: 0, periodsUnder30: 0 });
	});
});
