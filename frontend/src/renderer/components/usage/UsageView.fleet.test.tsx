import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useUiStore } from "../../stores/ui-store";
import { TooltipProvider } from "../ui/tooltip";
import { UsageFleetLimits } from "./UsageFleetLimits";
import { UsageView } from "./UsageView";

// Cross-host fixture (design §11, limits only): this computer plus five hosts.
// A: device login read 2 min ago. B: a saved account with the same email read
// 50 min ago. C: offline. D: refuses this device (re-pair). E: an old build (404).
const now = Date.now();
const ago = (minutes: number) => new Date(now - minutes * 60_000).toISOString();
const ahead = (hours: number) => new Date(now + hours * 3_600_000).toISOString();
const WEEKLY_RESET = ahead(50);

type Answer = { status: number; body: unknown } | "unreachable";
const accounts = (list: unknown[]): Answer => ({ status: 200, body: { accounts: list } });
const noHistory: Answer = { status: 200, body: { since: ago(10080), until: ago(0), logins: [] } };

const fixtures = vi.hoisted(() => ({ answers: {} as Record<string, Record<string, unknown>>, calls: [] as string[] }));

const hostAnswers: Record<string, Record<string, Answer>> = {
	local: {
		"/api/v1/agents/accounts": accounts([
			{ id: "device-codex", provider: "codex", device: true, default: true, loggedIn: true, signedIn: true, email: "markus.frommherz@gmail.com", plan: "pro", limits: { fetchedAt: ago(10), windows: [{ id: "codex:Weekly", label: "Weekly", usedPercent: 40, resetsAt: ahead(30), durationMinutes: 10080 }] } },
			{ id: "device-claude", provider: "claude", device: true, default: false, loggedIn: false, signedIn: false },
		]),
		"/api/v1/usage/limit-windows": noHistory,
	},
	"host-a": {
		"/api/v1/agents/accounts": accounts([
			{ id: "device-claude", provider: "claude", device: true, default: true, loggedIn: true, signedIn: true, email: "icosa2050@gmail.com", plan: "max", limits: { fetchedAt: ago(2), windows: [
				{ id: "seven_day", label: "Weekly", usedPercent: 31, resetsAt: WEEKLY_RESET, durationMinutes: 10080 },
				{ id: "model:Fable", label: "Weekly · Fable", usedPercent: 55, resetsAt: WEEKLY_RESET, durationMinutes: 10080 },
			] } },
		]),
		"/api/v1/usage/limit-windows": noHistory,
	},
	"host-b": {
		"/api/v1/agents/accounts": accounts([
			{ id: "acct-7", provider: "claude", device: false, default: true, loggedIn: true, signedIn: true, email: "ICOSA2050@gmail.com", plan: "max", limits: { fetchedAt: ago(50), windows: [
				{ id: "seven_day", label: "Weekly", usedPercent: 12, resetsAt: WEEKLY_RESET, durationMinutes: 10080 },
				{ id: "five_hour", label: "Session (5h)", usedPercent: 88, resetsAt: ago(5), durationMinutes: 300 },
			] } },
		]),
		// An old build without limit history.
		"/api/v1/usage/limit-windows": { status: 404, body: { code: "NOT_FOUND", message: "not found" } },
	},
	"host-d": {
		"/api/v1/agents/accounts": { status: 401, body: { code: "DEVICE_CREDENTIAL_REQUIRED", message: "agent accounts and usage require a paired device or account token" } },
		"/api/v1/usage/limit-windows": { status: 401, body: { code: "DEVICE_CREDENTIAL_REQUIRED", message: "x" } },
	},
	"host-e": {
		"/api/v1/agents/accounts": { status: 404, body: { code: "NOT_FOUND", message: "not found" } },
		"/api/v1/usage/limit-windows": { status: 404, body: { code: "NOT_FOUND", message: "not found" } },
	},
};

function fakeClient(hostId: string) {
	return {
		GET: async (path: string, options?: { params?: { query?: Record<string, unknown> } }) => {
			fixtures.calls.push(`${hostId} ${path}${options?.params?.query?.refresh ? "?refresh" : ""}`);
			const answer = hostAnswers[hostId]?.[path] ?? "unreachable";
			if (answer === "unreachable") throw new TypeError("Failed to fetch");
			const response = new Response(null, { status: answer.status === 401 ? 401 : answer.status });
			return answer.status < 300 ? { data: answer.body, response } : { error: answer.body, response };
		},
	};
}

const CONNECTED = ["host-a", "host-b", "host-d", "host-e"];
vi.mock("../../lib/host-clients", async (importOriginal) => ({
	...(await importOriginal<typeof import("../../lib/host-clients")>()),
	connectedHosts: () => CONNECTED,
	subscribeConnectedHosts: () => () => undefined,
	clientForHost: (hostId: string) => fakeClient(hostId),
}));
vi.mock("../../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../../lib/api-client")>()),
	apiClient: fakeClient("local"),
}));
const remoteHosts = [
	{ hostId: "host-a", label: "MacStudio", url: "https://a", status: "connected" },
	{ hostId: "host-b", label: "mac-mini", url: "https://b", status: "connected" },
	{ hostId: "host-c", label: "P50", url: "https://c", status: "offline" },
	{ hostId: "host-d", label: "MB (2)", url: "https://d", status: "connected" },
	{ hostId: "host-e", label: "Old host", url: "https://e", status: "connected" },
];
vi.mock("../../lib/shell-context", async (importOriginal) => ({
	...(await importOriginal<typeof import("../../lib/shell-context")>()),
	useShellMaybe: () => ({ remoteHosts }),
}));
vi.mock("../NotificationCenter", () => ({ NotificationCenter: () => null }));
vi.mock("../../hooks/useWorkspaceQuery", () => ({ useWorkspaceQuery: () => ({ data: [] }) }));
vi.mock("./UsageSpend", () => ({ UsageSpend: () => null }));

async function openLimits() {
	const user = userEvent.setup();
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	render(<QueryClientProvider client={client}><TooltipProvider><UsageView /></TooltipProvider></QueryClientProvider>);
	await user.click(within(screen.getByRole("group", { name: "Usage view" })).getByRole("button", { name: "Limits" }));
	const table = await screen.findByRole("table", { name: "Account limits" });
	await waitFor(() => expect(within(table).getAllByRole("row").length).toBeGreaterThan(4));
	return { user, table };
}

const rowsOf = (table: HTMLElement) => within(table).getAllByRole("row").slice(1);
const cells = (row: HTMLElement) => within(row).getAllByRole("cell").map((cell) => cell.textContent);

describe("UsageView fleet limits", () => {
	beforeEach(() => {
		fixtures.calls = [];
		useUiStore.setState({ developerMode: false, remoteHosts: true });
	});

	it("merges accounts across hosts into one row per account and window", async () => {
		const { table } = await openLimits();
		const claude = rowsOf(table).filter((row) => row.getAttribute("data-fleet-account")?.startsWith("claude\0icosa2050@gmail.com"));
		expect(claude.map((row) => row.getAttribute("data-fleet-window"))).toEqual(["five_hour", "seven_day", "model:Fable"]);
		const [fiveHour, weekly, fable] = claude.map(cells);
		// The freshest reading wins: A's 31 %, not B's 12 %.
		expect(weekly.slice(0, 5)).toEqual(["Claude Code", "icosa2050@gmail.com", "Max", "Weekly", "31%"]);
		expect(weekly[6]).toBe("2 minutes ago on MacStudio");
		expect(weekly[7]).toBe("MacStudio · mac-mini");
		expect(within(claude[1]).getByText((_, element) => element?.tagName === "TIME" && element.getAttribute("dateTime") === WEEKLY_RESET)).toBeInTheDocument();
		expect(fable[3]).toBe("Weekly · Fable");
		expect(fable[4]).toBe("55%");
		// B's 5h reading has reset: no stale level.
		expect(fiveHour[3]).toBe("5h");
		expect(fiveHour[4]).toBe("Reset · awaiting reading");
		expect(fiveHour[6]).toBe("50 minutes ago on mac-mini");
	});

	it("shows hosts that cannot answer as plain status text", async () => {
		await openLimits();
		const states = await screen.findByText(/P50: offline/);
		expect(states.textContent).toBe("P50: offline · MB (2): needs re-pair · Old host: needs update");
		// No email outside the limit rows.
		expect(states.textContent).not.toContain("@");
		// The unreachable host is never asked; the others are, once each.
		expect(fixtures.calls.filter((call) => call.startsWith("host-c"))).toEqual([]);
		expect(fixtures.calls.filter((call) => call === "host-d /api/v1/agents/accounts")).toHaveLength(1);
	});

	it("keeps login-less accounts per host and narrows to one host from the Hosts menu", async () => {
		const { user, table } = await openLimits();
		const signedOut = rowsOf(table).filter((row) => cells(row)[1] === "Claude Code CLI loginSigned out");
		expect(signedOut).toHaveLength(1);
		expect(cells(signedOut[0])[7]).toBe("This computer");

		await user.click(screen.getByRole("button", { name: "Hosts" }));
		await user.click(await screen.findByRole("menuitem", { name: "mac-mini" }));
		const narrowed = await screen.findByRole("table", { name: "Account limits" });
		await waitFor(() => expect(rowsOf(narrowed).map((row) => cells(row)[4])).toEqual(["Reset · awaiting reading", "12%"]));
		// Single host: no Hosts column, and the chip returns to all hosts.
		expect(within(narrowed).queryByRole("columnheader", { name: "Hosts" })).not.toBeInTheDocument();
		await user.click(screen.getByRole("button", { name: "Remove filter Host: mac-mini" }));
		expect(await screen.findByRole("columnheader", { name: "Hosts" })).toBeInTheDocument();
	});

	it("refreshes every reachable host in scope on an explicit click", async () => {
		const { user } = await openLimits();
		await user.click(screen.getByRole("button", { name: "Refresh" }));
		await waitFor(() => expect(fixtures.calls.filter((call) => call.endsWith("?refresh")).sort()).toEqual([
			"host-a /api/v1/agents/accounts?refresh",
			"host-b /api/v1/agents/accounts?refresh",
			"host-d /api/v1/agents/accounts?refresh",
			"host-e /api/v1/agents/accounts?refresh",
			"local /api/v1/agents/accounts?refresh",
		]));
	});

	it("says why a selected host shows nothing instead of asking to sign in", async () => {
		const { user } = await openLimits();
		await user.click(screen.getByRole("button", { name: "Hosts" }));
		await user.click(await screen.findByRole("menuitem", { name: "P50: offline" }));
		expect(await screen.findByText("Its limits appear here once the host answers again.")).toBeInTheDocument();
		expect(screen.getByText("P50: offline", { selector: "p" })).toBeInTheDocument();
		expect(screen.queryByText("No accounts report limits yet")).not.toBeInTheDocument();
		expect(screen.queryByRole("table")).not.toBeInTheDocument();

		await user.click(screen.getByRole("button", { name: /^Hosts/ }));
		await user.click(await screen.findByRole("menuitem", { name: "MB (2)" }));
		expect(await screen.findByText("Pair this device with the host again to read its limits.")).toBeInTheDocument();
	});

	it("says how old a stale host's reading is", () => {
		const client = new QueryClient();
		render(<QueryClientProvider client={client}><UsageFleetLimits rows={[]} showHosts now={new Date(now)} hosts={[
			{ hostId: "local", state: { kind: "ok" } },
			{ hostId: "host-a", label: "MacStudio", state: { kind: "stale", since: ago(5) } },
		]} /></QueryClientProvider>);
		expect(screen.getByText("MacStudio: stale, read 5 minutes ago")).toBeInTheDocument();
	});
});
