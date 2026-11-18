import { withoutSameLogins, type AgentAccount, type AgentAccountProvider } from "../hooks/useAgentAccounts";
import type { HostId } from "./hosts";
import type { UsageLimitLogin } from "./usage-limits";

/** Why a host in scope contributes no fresh reading. `ok` hosts are not listed. */
export type FleetHostState =
	| { kind: "ok" }
	| { kind: "loading" }
	| { kind: "connecting" }
	| { kind: "offline" }
	| { kind: "stale"; since: string | null }
	| { kind: "repair" }
	| { kind: "update" }
	| { kind: "error"; message: string };

export type FleetHostInput = {
	hostId: HostId;
	/** Undefined for this computer; the view supplies its translated name. */
	label?: string;
	state: FleetHostState;
	/** The host's last good answer from GET /agents/accounts. */
	accounts?: readonly AgentAccount[];
	/** The host's last good answer from GET /usage/limit-windows. */
	logins?: readonly UsageLimitLogin[];
};

export type FleetWindowKind = "5h" | "weekly" | "weekly-model" | "other";

export type FleetHostRef = { hostId: HostId; label?: string };

export type FleetLimitRow = {
	key: string;
	accountKey: string;
	provider: AgentAccountProvider;
	email: string | null;
	plan: string | null;
	/** The account as the host with the newest reading reports it, for its name and problems. */
	account: AgentAccount;
	/** Null when the account reports no window at all. */
	window: { id: string; label: string; kind: FleetWindowKind; model: string | null } | null;
	/** Null when the reading's window has already reset, or there is no reading. */
	usedPercent: number | null;
	resetsAt: string | null;
	/** True when resetsAt has passed: the shown level would be from the previous window. */
	resetPassed: boolean;
	readAt: string | null;
	readOn: FleetHostRef | null;
	hosts: FleetHostRef[];
};

/** Hosts answer with problems a person must see; HTTP status and error code decide which. */
export function hostStateFromError(error: unknown, hasData: boolean, dataUpdatedAt: number): FleetHostState {
	const { status, code } = (error ?? {}) as { status?: number; code?: string };
	if (code === "DEVICE_CREDENTIAL_REQUIRED" || code === "DEVICE_REVOKED" || status === 401) return { kind: "repair" };
	if (status === 404 || status === 501) return { kind: "update" };
	if (hasData) return { kind: "stale", since: dataUpdatedAt ? new Date(dataUpdatedAt).toISOString() : null };
	if (code === "UPSTREAM_UNAVAILABLE" || code === "HOST_IDENTITY_UNVERIFIED" || status === undefined || status >= 502) return { kind: "offline" };
	return { kind: "error", message: error instanceof Error ? error.message : String(error) };
}

const WEEK_MINUTES = 7 * 24 * 60;

/** 5h, weekly, weekly for one model (the label names it), or anything else. */
export function classifyWindow(window: { id: string; label: string; durationMinutes?: number }): { kind: FleetWindowKind; model: string | null } {
	const minutes = window.durationMinutes;
	if (window.id === "five_hour" || (minutes !== undefined && minutes > 0 && minutes <= 5 * 60)) return { kind: "5h", model: null };
	const weekly = window.id.startsWith("seven_day") || window.id.startsWith("model:") || (minutes !== undefined && minutes >= WEEK_MINUTES) || /\bweekly\b/i.test(window.label);
	if (!weekly) return { kind: "other", model: null };
	const model = window.label.replace(/\bweekly\b/i, "").replace(/^[\s·:-]+|[\s·:-]+$/g, "").trim();
	return model ? { kind: "weekly-model", model } : { kind: "weekly", model: null };
}

const PROVIDER_ORDER: AgentAccountProvider[] = ["codex", "claude"];
const KIND_ORDER: FleetWindowKind[] = ["5h", "weekly", "weekly-model", "other"];

type Reading = { usedPercent: number; resetsAt: string | null; readAt: string; host: FleetHostRef; fromAccounts: boolean };
type Merged = {
	accountKey: string;
	provider: AgentAccountProvider;
	email: string | null;
	/** Newest account record and when it was read, for plan and problems. */
	account: AgentAccount;
	accountReadAt: number;
	plan: string | null;
	hosts: FleetHostRef[];
	windows: Map<string, { label: string; durationMinutes?: number; reading: Reading | null }>;
};

const time = (iso: string | null | undefined) => (iso ? Date.parse(iso) || 0 : 0);

function newer(a: Reading | null, b: Reading): boolean {
	if (!a) return true;
	const diff = time(b.readAt) - time(a.readAt);
	// On a tie the account list wins: it is the host's current answer.
	return diff > 0 || (diff === 0 && b.fromAccounts && !a.fromAccounts);
}

/**
 * Merges every host's accounts into one row per account and limit window.
 * An account is its provider and email: limits are account-global, so the
 * newest reading from any host wins. An email that appears under two known
 * organisations stays split by organisation. Accounts without an email are
 * host-local and never merged.
 */
export function buildFleetLimitRows(hosts: readonly FleetHostInput[], now: Date): FleetLimitRow[] {
	const orgsByEmail = new Map<string, Set<string>>();
	for (const host of hosts) {
		for (const account of host.accounts ?? []) {
			if (!account.email || !account.orgId) continue;
			const base = `${account.provider}\0${account.email.toLowerCase()}`;
			orgsByEmail.set(base, (orgsByEmail.get(base) ?? new Set()).add(account.orgId));
		}
	}
	const keyOf = (hostId: HostId, account: Pick<AgentAccount, "provider" | "email" | "orgId" | "id">) => {
		if (!account.email) return `${account.provider}\0host\0${hostId}\0${account.id}`;
		const base = `${account.provider}\0${account.email.toLowerCase()}`;
		return (orgsByEmail.get(base)?.size ?? 0) > 1 ? `${base}\0${account.orgId ?? ""}` : base;
	};

	const merged = new Map<string, Merged>();
	for (const host of hosts) {
		const ref: FleetHostRef = { hostId: host.hostId, label: host.label };
		const accounts = withoutSameLogins([...(host.accounts ?? [])], host.accounts?.find((account) => account.default)?.id);
		const keyById = new Map<string, string>();
		for (const account of accounts) {
			const accountKey = keyOf(host.hostId, account);
			keyById.set(account.id, accountKey);
			const fetchedAt = account.limits?.fetchedAt ?? null;
			let entry = merged.get(accountKey);
			if (!entry) {
				entry = { accountKey, provider: account.provider, email: account.email ?? null, account, accountReadAt: -1, plan: null, hosts: [], windows: new Map() };
				merged.set(accountKey, entry);
			}
			if (!entry.hosts.some((known) => known.hostId === host.hostId)) entry.hosts.push(ref);
			if (time(fetchedAt) > entry.accountReadAt) {
				entry.account = account;
				entry.accountReadAt = time(fetchedAt);
				entry.plan = account.plan ?? entry.plan;
			} else if (!entry.plan && account.plan) {
				entry.plan = account.plan;
			}
			for (const window of account.limits?.windows ?? []) {
				const slot = entry.windows.get(window.id) ?? { label: window.label, durationMinutes: window.durationMinutes, reading: null };
				const reading: Reading = { usedPercent: window.usedPercent, resetsAt: window.resetsAt ?? null, readAt: fetchedAt ?? "", host: ref, fromAccounts: true };
				if (fetchedAt && newer(slot.reading, reading)) slot.reading = reading;
				entry.windows.set(window.id, slot);
			}
		}
		// Limit history adds readings for accounts the host still lists, never new accounts.
		for (const login of host.logins ?? []) {
			const accountKey = keyById.get(login.accountId);
			const entry = accountKey ? merged.get(accountKey) : undefined;
			if (!entry) continue;
			for (const span of login.windows) {
				const last = span.points?.at(-1);
				const reading: Reading = { usedPercent: last?.usedPercent ?? span.peakUsedPercent, resetsAt: span.resetsAt, readAt: last?.at ?? span.lastReadAt, host: ref, fromAccounts: false };
				const slot = entry.windows.get(span.windowId) ?? { label: span.label, durationMinutes: span.durationMinutes, reading: null };
				if (newer(slot.reading, reading)) slot.reading = reading;
				entry.windows.set(span.windowId, slot);
			}
		}
	}

	const rows: FleetLimitRow[] = [];
	for (const entry of merged.values()) {
		const base = { accountKey: entry.accountKey, provider: entry.provider, email: entry.email, plan: entry.plan, account: entry.account, hosts: entry.hosts };
		if (entry.windows.size === 0) {
			rows.push({ ...base, key: `${entry.accountKey}\0`, window: null, usedPercent: null, resetsAt: null, resetPassed: false, readAt: null, readOn: null });
			continue;
		}
		for (const [id, slot] of entry.windows) {
			const reading = slot.reading;
			const resetPassed = Boolean(reading?.resetsAt && time(reading.resetsAt) <= now.getTime());
			rows.push({
				...base,
				key: `${entry.accountKey}\0${id}`,
				window: { id, label: slot.label, ...classifyWindow({ id, label: slot.label, durationMinutes: slot.durationMinutes }) },
				usedPercent: reading && !resetPassed ? Math.max(0, Math.min(100, reading.usedPercent)) : null,
				resetsAt: reading?.resetsAt ?? null,
				resetPassed,
				readAt: reading?.readAt || null,
				readOn: reading?.host ?? null,
			});
		}
	}
	return rows.sort((a, b) =>
		PROVIDER_ORDER.indexOf(a.provider) - PROVIDER_ORDER.indexOf(b.provider)
		|| (a.email === null ? 1 : 0) - (b.email === null ? 1 : 0)
		|| (a.email ?? a.accountKey).localeCompare(b.email ?? b.accountKey)
		|| KIND_ORDER.indexOf(a.window?.kind ?? "other") - KIND_ORDER.indexOf(b.window?.kind ?? "other")
		|| (a.window?.label ?? "").localeCompare(b.window?.label ?? ""));
}
