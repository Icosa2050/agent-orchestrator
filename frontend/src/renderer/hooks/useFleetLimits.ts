import { useQueries, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import { apiClient, apiErrorCode } from "../lib/api-client";
import { apiErrorException } from "../lib/api-error-exception";
import { buildFleetLimitRows, hostStateFromError, type FleetHostInput, type FleetHostState } from "../lib/fleet-limits";
import { clientForHost } from "../lib/host-clients";
import { LOCAL_HOST, type HostId } from "../lib/hosts";
import { markApiError } from "../lib/reported-errors";
import { useShellMaybe } from "../lib/shell-context";
import type { UsageLimitWindows } from "../lib/usage-limits";
import { agentAccountsQueryKeyFor, type AgentAccount } from "./useAgentAccounts";
import { useConnectedHosts } from "./useHostConnection";
import type { RemoteHost } from "./useRemoteHosts";
import { usageReportQueryKey } from "./useUsageReport";

export const ALL_HOSTS = "all";
export type FleetScope = typeof ALL_HOSTS | HostId;

const LIMIT_WINDOW_DAYS = 7;
const client = (hostId: HostId) => (hostId === LOCAL_HOST ? apiClient : clientForHost(hostId));

/** A failed host request that keeps its HTTP status and error code, which decide the host's state. */
function hostError(error: unknown, status: number | undefined): Error {
	const exception = Object.assign(apiErrorException(error), { status, code: apiErrorCode(error) });
	markApiError(exception);
	return exception;
}

async function request<T>(call: () => Promise<{ data?: T; error?: unknown; response: Response }>): Promise<T> {
	let result: Awaited<ReturnType<typeof call>>;
	try {
		result = await call();
	} catch (error) {
		// No answer at all: the host or the proxy is unreachable.
		throw hostError(error, undefined);
	}
	if (result.error !== undefined || !result.response.ok) throw hostError(result.error ?? result.response.statusText, result.response.status);
	return result.data as T;
}

export async function fetchHostAccounts(hostId: HostId, refresh = false): Promise<AgentAccount[]> {
	const data = await request(() => client(hostId).GET("/api/v1/agents/accounts", { params: { query: refresh ? { refresh: true } : {} } }));
	return data?.accounts ?? [];
}

async function fetchHostLimitWindows(hostId: HostId): Promise<UsageLimitWindows> {
	return request(() => client(hostId).GET("/api/v1/usage/limit-windows", { params: { query: { days: LIMIT_WINDOW_DAYS } } })) as Promise<UsageLimitWindows>;
}

// This computer shares the Windows tab's cache entry; remote hosts get their own.
const limitWindowsKey = (hostId: HostId) =>
	hostId === LOCAL_HOST ? [...usageReportQueryKey, "limit-windows", LIMIT_WINDOW_DAYS] : [...usageReportQueryKey, "limit-windows", LIMIT_WINDOW_DAYS, "host", hostId];

// A host's 4xx answer (re-pair, update) is definitive; only a missing answer is retried.
const retryHost = (failures: number, error: unknown) => {
	const status = (error as { status?: number }).status;
	return failures < 1 && !(status !== undefined && status >= 400 && status < 500);
};

type QueryView<T> = { data?: T; error: unknown; isLoading: boolean; dataUpdatedAt: number; isFetching: boolean };

function hostState(query: QueryView<unknown> | undefined): FleetHostState {
	if (!query || query.isLoading) return { kind: "loading" };
	if (query.error) return hostStateFromError(query.error, query.data !== undefined, query.dataUpdatedAt);
	return { kind: "ok" };
}

/** Saved remote hosts with their connection state, plus any connected host not (yet) in the saved list. */
export function useFleetRemoteHosts(): Pick<RemoteHost, "hostId" | "label" | "status" | "failureReason">[] {
	const savedHosts = useShellMaybe()?.remoteHosts;
	const connected = useConnectedHosts();
	return useMemo(() => {
		const list = (savedHosts ?? []).map(({ hostId, label, status, failureReason }) => ({ hostId, label, status, failureReason }));
		for (const hostId of connected) if (!list.some((host) => host.hostId === hostId)) list.push({ hostId, label: hostId, status: "connected", failureReason: undefined });
		return list;
	}, [savedHosts, connected]);
}

/**
 * Account limits from this computer and every saved remote host in scope,
 * read through the desktop's remote-host proxy. Only connected hosts are asked;
 * the rest report why they are not.
 */
export function useFleetLimits(enabled: boolean, scope: FleetScope, now: Date) {
	const queryClient = useQueryClient();
	const connected = useConnectedHosts();
	const remotes = useFleetRemoteHosts();
	const inScope = useMemo(
		() => [LOCAL_HOST, ...remotes.map((host) => host.hostId)].filter((hostId) => scope === ALL_HOSTS || hostId === scope),
		[remotes, scope],
	);
	const reachable = inScope.filter((hostId) => hostId === LOCAL_HOST || connected.includes(hostId));

	const accountQueries = useQueries({
		queries: reachable.map((hostId) => ({
			queryKey: agentAccountsQueryKeyFor(hostId),
			queryFn: () => fetchHostAccounts(hostId),
			enabled,
			retry: retryHost,
			refetchInterval: 60_000,
		})),
	});
	const windowQueries = useQueries({
		queries: reachable.map((hostId) => ({
			queryKey: limitWindowsKey(hostId),
			queryFn: () => fetchHostLimitWindows(hostId),
			enabled,
			retry: retryHost,
			refetchInterval: 60_000,
		})),
	});

	const inputs: FleetHostInput[] = inScope.map((hostId) => {
		const remote = remotes.find((host) => host.hostId === hostId);
		const label = hostId === LOCAL_HOST ? undefined : remote?.label ?? hostId;
		const index = reachable.indexOf(hostId);
		if (index === -1) {
			const state: FleetHostState = remote?.failureReason === "unauthorized" ? { kind: "repair" }
				: remote?.failureReason === "incompatible" ? { kind: "update" }
				: remote?.status === "connecting" ? { kind: "connecting" }
				: { kind: "offline" };
			return { hostId, label, state };
		}
		const accounts = accountQueries[index];
		const windows = windowQueries[index];
		// History is optional: a host without it still shows its accounts' current readings.
		return { hostId, label, state: hostState(accounts), accounts: accounts?.data, logins: windows?.data?.logins };
	});

	const rows = buildFleetLimitRows(inputs, now);

	return {
		hosts: inputs,
		rows,
		isLoading: accountQueries.some((query) => query.isLoading) && rows.length === 0,
		isFetching: accountQueries.some((query) => query.isFetching),
		/** Asks every reachable host in scope for a fresh provider reading. Explicit clicks only. */
		refresh: async () => {
			const results = await Promise.allSettled(reachable.map(async (hostId) => {
				queryClient.setQueryData(agentAccountsQueryKeyFor(hostId), await fetchHostAccounts(hostId, true));
				await queryClient.invalidateQueries({ queryKey: limitWindowsKey(hostId), exact: true });
			}));
			const failed = results.find((result) => result.status === "rejected");
			if (failed) throw failed.reason;
		},
	};
}
