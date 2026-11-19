import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { buildPeakHistoryRows, summarizePeaks, type UsageLimitPeak } from "../../lib/limit-peaks";
import { UsageLimitPeaks } from "./UsageLimitPeaks";

const day = (n: number) => new Date(Date.UTC(2026, 10, n, 8)).toISOString();
const period = (reset: number, peak: number, open = false): UsageLimitPeak => ({
	resetAt: day(reset), peakUsedPct: peak, readings: 100, firstReadingAt: day(reset - 7), lastReadingAt: day(reset - 0.01), open,
});
const periods = [period(1, 10), period(2, 20), period(3, 30), period(4, 40), period(5, 50), period(6, 85), period(7, 12, true)];
const rows = buildPeakHistoryRows([
	{ hostId: "local", windows: [{
		provider: "claude", login: "me@example.com", email: "me@example.com", accountId: "device-claude", plan: "max",
		windowId: "model:Fable", label: "Weekly · Fable", durationMinutes: 10080, periods, days: [], summary: summarizePeaks(periods),
	}] },
	{ hostId: "host-a", label: "MacStudio", windows: [] },
]);

describe("UsageLimitPeaks", () => {
	it("lists the latest periods' peaks and the summary as plain numbers", () => {
		render(<UsageLimitPeaks rows={rows} weeks={12} showHosts />);
		expect(screen.getByRole("heading", { name: "History" })).toBeInTheDocument();
		const table = screen.getByRole("table", { name: "Limit history" });
		const [row] = within(table).getAllByRole("row").slice(1);
		const cells = within(row).getAllByRole("cell").map((cell) => cell.textContent);
		expect(cells[0]).toBe("me@example.comMax");
		expect(cells[1]).toBe("Weekly · Fable");
		// Six latest periods, oldest first; the open one is marked.
		const shown = row.querySelectorAll("[data-peak-period]");
		expect([...shown].map((cell) => cell.getAttribute("data-peak-period"))).toEqual([day(2), day(3), day(4), day(5), day(6), day(7)]);
		expect(shown[5].textContent).toContain("12% (still open)");
		expect(shown[5].getAttribute("title")).toContain("still open");
		expect(cells.slice(3)).toEqual(["6", "39%", "85%", "1", "2", "This computer"]);
	});

	it("shows nothing without history", () => {
		const { container } = render(<UsageLimitPeaks rows={[]} weeks={12} showHosts={false} />);
		expect(container).toBeEmptyDOMElement();
	});
});
