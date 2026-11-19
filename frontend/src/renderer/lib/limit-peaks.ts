import type { components } from "../../api/schema";
import type { AgentAccountProvider } from "../hooks/useAgentAccounts";
import { classifyWindow, type FleetHostRef, type FleetWindowKind } from "./fleet-limits";
import type { HostId } from "./hosts";

export type UsageLimitPeaks = components["schemas"]["UsageLimitPeaksResponse"];
export type UsageLimitPeakTrack = components["schemas"]["UsageLimitPeakTrack"];
export type UsageLimitPeak = components["schemas"]["UsageLimitPeak"];
export type UsageLimitPeakSummary = components["schemas"]["UsageLimitPeakSummary"];

export type PeakHostInput = { hostId: HostId; label?: string; windows?: readonly UsageLimitPeakTrack[] };

export type PeakHistoryRow = {
	key: string;
	provider: AgentAccountProvider;
	/** Null for a login the provider reported no email for. */
	email: string | null;
	plan: string | null;
	window: { id: string; label: string; kind: FleetWindowKind; model: string | null };
	/** Oldest first. */
	periods: UsageLimitPeak[];
	summary: UsageLimitPeakSummary;
	hosts: FleetHostRef[];
};

/** Weeks of reset periods the Limits tab asks each host for. */
export const LIMIT_PEAK_WEEKS = 12;

/** Providers round reset times; resets this close are the same reset. */
const SAME_RESET_MS = 2 * 60_000;
const PROVIDER_ORDER: AgentAccountProvider[] = ["codex", "claude"];
const KIND_ORDER: FleetWindowKind[] = ["5h", "weekly", "weekly-model", "other"];
const time = (iso: string) => Date.parse(iso) || 0;

/** The same rules as the daemon's summary: closed periods only. */
export function summarizePeaks(periods: readonly UsageLimitPeak[]): UsageLimitPeakSummary {
	const closed = periods.filter((period) => !period.open);
	const peaks = closed.map((period) => period.peakUsedPct);
	return {
		periods: closed.length,
		avgPeak: peaks.length ? peaks.reduce((sum, peak) => sum + peak, 0) / peaks.length : 0,
		maxPeak: peaks.length ? Math.max(...peaks) : 0,
		periodsOver80: peaks.filter((peak) => peak >= 80).length,
		periodsUnder30: peaks.filter((peak) => peak < 30).length,
	};
}

/** Two hosts saw the same period when its reset matches or their readings overlap in time (one host saw the reset move). */
function samePeriod(a: UsageLimitPeak, b: UsageLimitPeak): boolean {
	if (Math.abs(time(a.resetAt) - time(b.resetAt)) <= SAME_RESET_MS) return true;
	return time(a.firstReadingAt) <= time(b.lastReadingAt) && time(b.firstReadingAt) <= time(a.lastReadingAt);
}

/** The highest peak wins; the freshest reading names the reset. */
function mergePeriod(a: UsageLimitPeak, b: UsageLimitPeak): UsageLimitPeak {
	const fresher = time(b.lastReadingAt) > time(a.lastReadingAt) ? b : a;
	return {
		resetAt: fresher.resetAt,
		open: fresher.open,
		peakUsedPct: Math.max(a.peakUsedPct, b.peakUsedPct),
		readings: a.readings + b.readings,
		firstReadingAt: time(a.firstReadingAt) <= time(b.firstReadingAt) ? a.firstReadingAt : b.firstReadingAt,
		lastReadingAt: fresher.lastReadingAt,
	};
}

/**
 * One row per login and limit window across the hosts in scope. A login is
 * its provider and email, like the current limits table; a login without an
 * email stays per host. Each host's periods for the same reset merge into one.
 */
export function buildPeakHistoryRows(hosts: readonly PeakHostInput[]): PeakHistoryRow[] {
	// Each merged period remembers which hosts it came from: one host's own periods never merge.
	type Slot = { period: UsageLimitPeak; hosts: Set<HostId> };
	const rows = new Map<string, Omit<PeakHistoryRow, "periods"> & { slots: Slot[]; freshest: number }>();
	for (const host of hosts) {
		const ref: FleetHostRef = { hostId: host.hostId, label: host.label };
		for (const track of host.windows ?? []) {
			const login = track.email ? track.email.toLowerCase() : `host\0${host.hostId}\0${track.login}`;
			const key = `${track.provider}\0${login}\0${track.windowId}`;
			const latest = Math.max(0, ...track.periods.map((period) => time(period.lastReadingAt)));
			let row = rows.get(key);
			if (!row) {
				row = {
					key, provider: track.provider, email: track.email ?? null, plan: track.plan ?? null, slots: [], hosts: [], freshest: -1,
					window: { id: track.windowId, label: track.label, ...classifyWindow({ id: track.windowId, label: track.label, durationMinutes: track.durationMinutes }) },
					summary: summarizePeaks([]),
				};
				rows.set(key, row);
			}
			if (!row.hosts.some((known) => known.hostId === host.hostId)) row.hosts.push(ref);
			if (latest > row.freshest) {
				row.freshest = latest;
				row.plan = track.plan ?? row.plan;
			}
			for (const period of track.periods) {
				const slot = row.slots.find((known) => !known.hosts.has(host.hostId) && samePeriod(known.period, period));
				if (!slot) row.slots.push({ period, hosts: new Set([host.hostId]) });
				else {
					slot.period = mergePeriod(slot.period, period);
					slot.hosts.add(host.hostId);
				}
			}
		}
	}
	const out: PeakHistoryRow[] = [];
	for (const { freshest: _freshest, slots, ...row } of rows.values()) {
		const periods = slots.map((slot) => slot.period).sort((a, b) => time(a.resetAt) - time(b.resetAt));
		out.push({ ...row, periods, summary: summarizePeaks(periods) });
	}
	return out.sort((a, b) =>
		PROVIDER_ORDER.indexOf(a.provider) - PROVIDER_ORDER.indexOf(b.provider)
		|| (a.email === null ? 1 : 0) - (b.email === null ? 1 : 0)
		|| (a.email ?? a.key).localeCompare(b.email ?? b.key)
		|| KIND_ORDER.indexOf(a.window.kind) - KIND_ORDER.indexOf(b.window.kind)
		|| a.window.label.localeCompare(b.window.label));
}
