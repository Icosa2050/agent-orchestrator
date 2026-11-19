import { ChevronLeft, ChevronRight, Gauge, RefreshCw } from "lucide-react";
import { Filter } from "@/components/app-icons";
import { ScrollArea } from "radix-ui";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { LOCAL_DISTRIBUTION } from "../../../shared/fork-identity";
import { ALL_HOSTS, useFleetLimits, useFleetRemoteHosts, type FleetScope } from "../../hooks/useFleetLimits";
import { useUsageLimitWindows, useUsageReport } from "../../hooks/useUsageReport";
import { useWorkspaceQuery } from "../../hooks/useWorkspaceQuery";
import { LOCAL_HOST } from "../../lib/hosts";
import { formatRange } from "../../lib/usage-format";
import type { UsageUnit } from "../../lib/usage-report";
import { cn } from "../../lib/utils";
import { useUiStore } from "../../stores/ui-store";
import { CLOUD_PROJECT_KIND, STANDALONE_WORKSPACE_ID } from "../../types/workspace";
import { SettingsOptionMenu } from "../settings/SettingsOptionMenu";
import { PageTopbar, PageTopbarTitle } from "../PageTopbar";
import { FilterChip } from "../PageToolbar";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuLabel,
	DropdownMenuRadioGroup,
	DropdownMenuRadioItem,
	DropdownMenuTrigger,
} from "../ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipTrigger } from "../ui/tooltip";
import { UsageFleetLimits } from "./UsageFleetLimits";
import { LIMIT_PEAK_WEEKS } from "../../lib/limit-peaks";
import { UsageLimitPeaks } from "./UsageLimitPeaks";
import { UsageSkeleton } from "./UsageSkeleton";
import { UsageSpend } from "./UsageSpend";
import { UsageWindows } from "./UsageWindows";

type UsageTab = "spend" | "limits" | "windows";
const ALL_PROJECTS = "all";
const PERIODS = [
	{ days: 1, label: "usage.period.day" },
	{ days: 7, label: "usage.period.week" },
	{ days: 30, label: "usage.period.month" },
	{ days: 90, label: "usage.period.quarter" },
] as const;

function useNow(intervalMs: number): Date {
	const [now, setNow] = useState(() => new Date());
	useEffect(() => {
		const id = window.setInterval(() => setNow(new Date()), intervalMs);
		return () => window.clearInterval(id);
	}, [intervalMs]);
	return now;
}

export function UsageView() {
	const { t, i18n } = useTranslation();
	const [chosenTab, setTab] = useState<UsageTab>("spend");
	const developerMode = useUiStore((state) => state.developerMode);
	const showGlobalToast = useUiStore((state) => state.showGlobalToast);
	const tab = chosenTab === "windows" && !developerMode ? "spend" : chosenTab;
	// Fork (Icosa2050): the local build leads with raw token counts, not cost estimates.
	const [unit, setUnit] = useState<UsageUnit>(LOCAL_DISTRIBUTION ? "tokens" : "cost");
	const [days, setDays] = useState(7);
	const [offset, setOffset] = useState(0);
	const [projectFilter, setProjectFilter] = useState(ALL_PROJECTS);
	const [refreshingLimits, setRefreshingLimits] = useState(false);
	const [chosenScope, setScope] = useState<FleetScope>(ALL_HOSTS);
	const now = useNow(30_000);
	const remoteHosts = useFleetRemoteHosts();
	// Without a saved remote the page is this computer's, as before.
	const scope: FleetScope = remoteHosts.length === 0 ? LOCAL_HOST
		: chosenScope === ALL_HOSTS || chosenScope === LOCAL_HOST || remoteHosts.some((host) => host.hostId === chosenScope) ? chosenScope : ALL_HOSTS;
	const hostLabel = (hostId: string) => hostId === LOCAL_HOST ? t("remote.thisComputer") : remoteHosts.find((host) => host.hostId === hostId)?.label ?? hostId;
	const hostOptionState = (host: (typeof remoteHosts)[number]) =>
		host.failureReason === "unauthorized" ? t("usage.fleet.state.repair")
			: host.failureReason === "incompatible" ? t("usage.fleet.state.update")
			: host.status === "connected" ? null : t(`usage.fleet.state.${host.status}`);
	const scopeOptions = [
		{ value: ALL_HOSTS, label: t("usage.fleet.allHosts") },
		{ value: LOCAL_HOST, label: t("remote.thisComputer") },
		...remoteHosts.map((host) => {
			const state = hostOptionState(host);
			return { value: host.hostId, label: state ? t("usage.fleet.hostState", { host: host.label, state }) : host.label };
		}),
	];

	const workspaces = useWorkspaceQuery({ includeCloud: false }).data;
	const projects = useMemo(
		() => (workspaces ?? []).filter((workspace) => !workspace.hostId && workspace.kind !== CLOUD_PROJECT_KIND),
		[workspaces],
	);
	const projectNames = useMemo(
		() => new Map(projects.map((project) => [project.id, project.id === STANDALONE_WORKSPACE_ID ? t("standalone.workspaceName") : project.name])),
		[projects, t],
	);
	const filterProjects = useMemo(() => projects.filter((project) => project.id !== STANDALONE_WORKSPACE_ID), [projects]);
	const report = useUsageReport(days, offset, projectFilter === ALL_PROJECTS ? undefined : projectFilter);
	const fleet = useFleetLimits(tab === "limits", scope, now);
	const limitWindows = useUsageLimitWindows(7, tab === "windows");
	const isLimits = tab === "limits";
	const isWindows = tab === "windows";

	const refresh = async () => {
		if (isWindows) {
			await limitWindows.refetch();
			return;
		}
		if (!isLimits) {
			await report.refetch();
			return;
		}
		setRefreshingLimits(true);
		try {
			await fleet.refresh();
		} catch (error) {
			showGlobalToast(t("usage.limits.refreshFailed"), error instanceof Error ? error.message : undefined, "error");
		} finally {
			setRefreshingLimits(false);
		}
	};
	const refreshing = isWindows ? limitWindows.isFetching : isLimits ? refreshingLimits || fleet.isFetching : report.isFetching;
	const loading = <UsageSkeleton view={tab} />;
	const failed = (error: Error) => <p role="alert" className="py-16 text-center text-sm text-destructive">{error.message}</p>;
	const staleError = (error: Error | null) => (error ? <p role="alert" className="mb-4 text-xs text-destructive">{error.message}</p> : null);
	const range = report.data ? formatRange(report.data.since, report.data.until, i18n.language) : null;
	const views: UsageTab[] = developerMode ? ["spend", "limits", "windows"] : ["spend", "limits"];
	const selectedProjectName = projectFilter === ALL_PROJECTS ? null : projectNames.get(projectFilter) ?? projectFilter;

	return (
		<div className="flex min-h-0 flex-1 flex-col bg-background">
			<PageTopbar lead={<PageTopbarTitle as="h1" icon={<Gauge className="size-icon-md" aria-hidden="true" />}>{t("usage.title")}</PageTopbarTitle>} />
			<div className="shrink-0">
				<div className="mx-auto w-full max-w-6xl px-6 pt-3">
					<div className="flex min-h-control-md items-center gap-2">
						<div role="group" aria-label={t("usage.views")} className="flex min-w-0 items-center gap-1">
							{views.map((view) => (
								<Button key={view} type="button" size="sm" variant="ghost" aria-pressed={tab === view} onClick={() => setTab(view)} className={view === "windows" ? "gap-1.5" : undefined}>
									{t(`usage.view.${view}`)}
									{view === "windows" ? <Badge className="h-4 px-1.5 text-2xs font-normal">{t("usage.windows.experimental")}</Badge> : null}
								</Button>
							))}
						</div>
						<div className="ms-auto flex shrink-0 items-center gap-0.5">
							{tab === "limits" && remoteHosts.length > 0 ? (
								<SettingsOptionMenu
									aria-label={t("usage.fleet.scope")}
									value={scope}
									options={scopeOptions}
									onChange={(value) => setScope(value)}
									menuAlign="end"
									triggerClassName="h-(--size-control-md)! text-xs!"
									renderTrigger={(selected) => (
										<span className="min-w-0 truncate">
											<span className="text-muted-foreground">{t("usage.fleet.scope")} </span>
											{selected?.label}
										</span>
									)}
								/>
							) : null}
							{tab === "spend" && filterProjects.length > 0 ? (
								<DropdownMenu>
									<Tooltip>
										<TooltipTrigger asChild>
											<DropdownMenuTrigger asChild>
												<Button type="button" size="icon-sm" variant="quiet" aria-label={t("usage.filter")} data-active={projectFilter !== ALL_PROJECTS ? "true" : undefined}>
													<Filter aria-hidden="true" className="size-icon-sm" />
												</Button>
											</DropdownMenuTrigger>
										</TooltipTrigger>
										<TooltipContent side="bottom">{t("usage.filter")}</TooltipContent>
									</Tooltip>
									<DropdownMenuContent align="end" className="max-h-72 min-w-52 overflow-y-auto" data-browser-native-overlay="true">
										<DropdownMenuLabel>{t("usage.projectFilter")}</DropdownMenuLabel>
										<DropdownMenuRadioGroup value={projectFilter} onValueChange={setProjectFilter}>
											<DropdownMenuRadioItem value={ALL_PROJECTS}>{t("usage.allProjects")}</DropdownMenuRadioItem>
											{filterProjects.map((project) => <DropdownMenuRadioItem key={project.id} value={project.id}><span className="truncate">{projectNames.get(project.id) ?? project.name}</span></DropdownMenuRadioItem>)}
										</DropdownMenuRadioGroup>
									</DropdownMenuContent>
								</DropdownMenu>
							) : null}
							{tab === "spend" ? (
								<div className="flex items-center gap-0.5">
									<Button type="button" variant="quiet" size="icon-sm" aria-label={t("usage.period.previous")} onClick={() => setOffset((value) => value + 1)}>
										<ChevronLeft aria-hidden="true" className="size-icon-sm" />
									</Button>
									<SettingsOptionMenu
										aria-label={t("usage.period")}
										value={String(days)}
										options={PERIODS.map((period) => ({ value: String(period.days), label: t(period.label) }))}
										onChange={(value) => {
											setDays(Number(value));
											setOffset(0);
										}}
										menuAlign="end"
										triggerClassName="h-(--size-control-md)! text-xs!"
										renderTrigger={(selected) => (
											<span className="min-w-0 truncate">
												{offset === 0 || !range ? selected?.label : null}
												{range ? <span className="text-muted-foreground">{offset === 0 ? ` · ${range}` : range}</span> : null}
											</span>
										)}
									/>
									<Button type="button" variant="quiet" size="icon-sm" aria-label={t("usage.period.next")} disabled={offset === 0} onClick={() => setOffset((value) => Math.max(0, value - 1))}>
										<ChevronRight aria-hidden="true" className="size-icon-sm" />
									</Button>
								</div>
							) : null}
							<Tooltip>
								<TooltipTrigger asChild>
									<Button type="button" variant="quiet" size="icon-sm" aria-label={t("usage.refresh")} disabled={refreshing} onClick={() => void refresh()}>
										<RefreshCw aria-hidden="true" className={cn("size-icon-sm", refreshing && "animate-spin motion-reduce:animate-none")} />
									</Button>
								</TooltipTrigger>
								<TooltipContent side="bottom">{t("usage.refresh")}</TooltipContent>
							</Tooltip>
						</div>
					</div>
					{tab === "limits" && remoteHosts.length > 0 && scope !== ALL_HOSTS ? (
						<div className="mt-2 flex flex-wrap items-center gap-1.5">
							<FilterChip
								label={t("usage.fleet.host")}
								value={hostLabel(scope)}
								removeLabel={t("usage.removeFilter", { filter: `${t("usage.fleet.host")}: ${hostLabel(scope)}` })}
								onRemove={() => setScope(ALL_HOSTS)}
							/>
						</div>
					) : null}
					{tab === "spend" && selectedProjectName ? (
						<div className="mt-2 flex flex-wrap items-center gap-1.5">
							<FilterChip
								label={t("usage.projectFilter")}
								value={selectedProjectName}
								removeLabel={t("usage.removeFilter", { filter: `${t("usage.projectFilter")}: ${selectedProjectName}` })}
								onRemove={() => setProjectFilter(ALL_PROJECTS)}
							/>
						</div>
					) : null}
				</div>
			</div>
			<ScrollArea.Root asChild type="hover" scrollHideDelay={500}>
				<main className="relative min-h-0 flex-1 overflow-hidden">
					<ScrollArea.Viewport className="h-full w-full overscroll-contain focus-visible:outline-none">
						<div className="mx-auto w-full max-w-6xl px-6 pt-5 pb-6">
							{isWindows ? (
								limitWindows.data ? (
									<>
										{staleError(limitWindows.error)}
										<UsageWindows history={limitWindows.data} now={now} />
									</>
								) : limitWindows.isLoading ? (
									loading
								) : limitWindows.error ? (
									failed(limitWindows.error)
								) : null
							) : isLimits ? (
								fleet.isLoading ? (
									loading
								) : (
									<>
										<UsageFleetLimits rows={fleet.rows} hosts={fleet.hosts} showHosts={remoteHosts.length > 0 && scope === ALL_HOSTS} now={now} />
										<UsageLimitPeaks rows={fleet.history} weeks={LIMIT_PEAK_WEEKS} showHosts={remoteHosts.length > 0 && scope === ALL_HOSTS} />
									</>
								)
							) : report.data && report.data.rows.length > 0 ? (
								<>
									{staleError(report.error)}
									<div className={cn("transition-opacity", report.isPlaceholderData && "opacity-60")}>
										<UsageSpend report={report.data} unit={unit} onUnitChange={setUnit} projectNames={projectNames} />
									</div>
								</>
							) : report.isLoading ? (
								loading
							) : report.error ? (
								failed(report.error)
							) : (
								<div className="flex flex-col items-center gap-1 py-16 text-center">
									<p className="text-control font-medium text-foreground">{t("usage.empty.title")}</p>
									<p className="max-w-sm text-xs text-muted-foreground">{t("usage.empty.body")}</p>
								</div>
							)}
						</div>
					</ScrollArea.Viewport>
					<ScrollArea.Scrollbar orientation="vertical" className="z-10 flex w-2 touch-none select-none p-0.5">
						<ScrollArea.Thumb className="relative flex-1 rounded-full bg-[color-mix(in_srgb,var(--color-scrollbar)_42%,transparent)] hover:bg-[color-mix(in_srgb,var(--color-scrollbar)_56%,transparent)]" />
					</ScrollArea.Scrollbar>
				</main>
			</ScrollArea.Root>
		</div>
	);
}
