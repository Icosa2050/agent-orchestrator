import { describe, expect, it } from "vitest";
import { formatCountdown, spanPace, usedPercent, type UsageLimitSpan } from "./usage-limits";

describe("usedPercent", () => {
	it("is how much of the window is consumed, clamped to 0-100", () => {
		expect(usedPercent(18)).toBe(18);
		expect(usedPercent(130)).toBe(100);
		expect(usedPercent(-5)).toBe(0);
	});
});

describe("formatCountdown", () => {
	const now = new Date("2026-10-25T10:00:00Z");
	it("uses the two largest units", () => {
		expect(formatCountdown("2026-10-25T13:45:00Z", now)).toBe("3h 45m");
		expect(formatCountdown("2026-10-31T10:00:00Z", now)).toBe("6d 0h");
		expect(formatCountdown("2026-10-25T10:11:30Z", now)).toBe("12m");
	});
	it("returns nothing for a passed or missing reset", () => {
		expect(formatCountdown("2026-10-25T09:00:00Z", now)).toBeNull();
		expect(formatCountdown(null, now)).toBeNull();
	});
});

describe("spanPace", () => {
	const span = (overrides: Partial<UsageLimitSpan>): UsageLimitSpan => ({
		windowId: "codex:Weekly", label: "Weekly", durationMinutes: 10080, state: "open", readings: 2,
		startsAt: "2026-10-28T06:35:00Z", resetsAt: "2026-11-04T06:35:00Z",
		firstReadAt: "2026-10-28T07:00:00Z", lastReadAt: "2026-10-29T06:35:00Z", peakUsedPercent: 20, ...overrides,
	});

	it("projects the pace since the window opened to the reset", () => {
		const pace = spanPace(span({ peakUsedPercent: 10 }));
		expect(pace?.atReset).toBeCloseTo(70);
		expect(pace?.runsOutAt).toBeNull();
		const fast = spanPace(span({ points: [{ at: "2026-10-29T06:35:00Z", usedPercent: 25 }] }));
		expect(fast?.atReset).toBe(100);
		expect(Math.abs((fast?.runsOutAt?.getTime() ?? 0) - Date.parse("2026-11-01T06:35:00Z"))).toBeLessThan(1000);
	});

	it("says nothing in the first hour or without a start", () => {
		expect(spanPace(span({ lastReadAt: "2026-10-28T07:00:00Z" }))).toBeNull();
		expect(spanPace(span({ startsAt: null }))).toBeNull();
	});
});
