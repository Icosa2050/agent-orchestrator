// Fork guard (Icosa2050): the packaged local-distribution build exposes the
// Usage page and the Cmd+K palette without Developer Mode, while Teams and the
// experimental Windows tab stay off. Run by scripts/fork/protected-tests.sh.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { type ComponentType, Suspense } from "react";
import { beforeEach, expect, it, vi } from "vitest";
import { LOCAL_DISTRIBUTION } from "../shared/fork-identity";
import { UsageView } from "./components/usage/UsageView";
import { TooltipProvider } from "./components/ui/tooltip";
import { isCommandPaletteEnabled, isTeamsEnabled } from "./lib/build-channel";
import { buildCommands } from "./lib/command-palette";
import { Route as UsageRoute } from "./routes/_shell.usage";
import { useUiStore } from "./stores/ui-store";

vi.mock("./components/NotificationCenter", () => ({ NotificationCenter: () => null }));
vi.mock("./hooks/useWorkspaceQuery", () => ({ useWorkspaceQuery: () => ({ data: [] }) }));
vi.mock("./hooks/useUsageReport", () => ({
	useUsageReport: () => ({
		data: { since: "2026-11-10T00:00:00Z", until: "2026-11-17T00:00:00Z", periods: [], rows: [{ harness: "claude-code", model: "m", tokens: 1 }], sessions: [], totals: {} },
		isLoading: false,
		isFetching: false,
		error: null,
		isPlaceholderData: false,
		refetch: vi.fn(),
	}),
	useUsageLimitWindows: () => ({ data: undefined, isLoading: false, isFetching: false, error: null, refetch: vi.fn() }),
}));
vi.mock("./hooks/useAgentAccounts", async (importOriginal) => ({
	...await importOriginal<typeof import("./hooks/useAgentAccounts")>(),
	useAgentAccounts: () => ({ data: [], isLoading: false, isFetching: false, error: null }),
	useAgentAccountActions: () => ({ refresh: vi.fn() }),
}));
vi.mock("./components/usage/UsageSpend", () => ({ UsageSpend: ({ unit }: { unit: string }) => <p>Spend unit {unit}</p> }));

const PACKAGED = "0.14.8-icosa.2";

beforeEach(() => useUiStore.setState({ developerMode: false }));

it("is the local-distribution build", () => {
	expect(LOCAL_DISTRIBUTION).toBe(true);
});

it("enables the command palette in the packaged build but keeps Teams off", () => {
	expect(isCommandPaletteEnabled(PACKAGED, false)).toBe(true);
	expect(isTeamsEnabled(PACKAGED, false, "")).toBe(false);
});

it("offers the Usage command in the palette without Developer Mode", () => {
	const usage = buildCommands({ workspaces: [] }).find((item) => item.id === "global-usage");
	expect(usage?.action).toEqual({ kind: "navigate", target: { to: "/usage" } });
});

function renderPage(Page: ComponentType) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	render(<QueryClientProvider client={client}><TooltipProvider><Suspense><Page /></Suspense></TooltipProvider></QueryClientProvider>);
}

it("routes /usage to the Usage page without Developer Mode", async () => {
	const Page = UsageRoute.options.component;
	expect(Page).toBeDefined();
	await (Page as { preload?: () => Promise<void> }).preload?.();
	renderPage(Page as ComponentType);
	expect(await screen.findByRole("heading", { name: "Usage", level: 1 })).toBeInTheDocument();
});

it("shows Spend and Limits in raw tokens and keeps Windows behind Developer Mode", () => {
	renderPage(UsageView);
	const views = screen.getByRole("group", { name: "Usage view" });
	expect(within(views).getAllByRole("button").map((button) => button.textContent)).toEqual(["Spend", "Limits"]);
	expect(screen.getByText("Spend unit tokens")).toBeInTheDocument();
});
