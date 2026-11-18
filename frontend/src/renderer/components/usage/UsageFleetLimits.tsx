import { useTranslation } from "react-i18next";
import { agentAccountName, agentAccountProviderNames, describeAccountProblem } from "../../hooks/useAgentAccounts";
import type { FleetHostInput, FleetHostRef, FleetHostState, FleetLimitRow } from "../../lib/fleet-limits";
import { PROVIDER_HARNESS } from "../../lib/usage-limits";
import { formatRelativeTime } from "../../lib/usage-format";
import { useUiStore } from "../../stores/ui-store";
import { AgentAvatar } from "../AgentAvatar";
import { formatPlanName } from "../settings/AgentAccountsSection";
import { Button } from "../ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";

/**
 * Account limits as raw numbers: one row per account and limit window, merged
 * across the hosts in scope. Hosts that could not answer are listed as plain
 * status text above the table.
 */
export function UsageFleetLimits({ rows, hosts, showHosts, now }: { rows: readonly FleetLimitRow[]; hosts: readonly FleetHostInput[]; showHosts: boolean; now: Date }) {
	const { t, i18n } = useTranslation();
	const openGlobalSettings = useUiStore((state) => state.openGlobalSettings);
	const hostName = (host: FleetHostRef) => host.label ?? t("remote.thisComputer");
	const percent = new Intl.NumberFormat(i18n.language, { style: "percent", maximumFractionDigits: 0 });
	const instant = new Intl.DateTimeFormat(i18n.language, { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
	const absolute = new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short" });
	const stateText = (state: FleetHostState) => {
		switch (state.kind) {
			case "stale": return state.since ? t("usage.fleet.state.staleSince", { value: formatRelativeTime(state.since, now, i18n.language) }) : t("usage.fleet.state.stale");
			case "error": return t("usage.fleet.state.error", { message: state.message });
			case "ok": return "";
			default: return t(`usage.fleet.state.${state.kind}`);
		}
	};
	const troubled = hosts.filter((host) => host.state.kind !== "ok");
	// A single selected host that cannot answer: say so instead of "sign in".
	const unavailable = rows.length === 0 && hosts.length === 1 && troubled.length === 1 && hosts[0].label !== undefined ? hosts[0] : null;
	const unavailableBody = (state: FleetHostState) =>
		state.kind === "repair" ? t("usage.fleet.unavailable.repair") : state.kind === "update" ? t("usage.fleet.unavailable.update") : t("usage.fleet.unavailable.other");
	const windowName = (window: NonNullable<FleetLimitRow["window"]>) =>
		window.kind === "5h" ? t("usage.fleet.window.fiveHour")
			: window.kind === "weekly" ? t("usage.fleet.window.weekly")
			: window.kind === "weekly-model" && window.model ? t("usage.fleet.window.weeklyModel", { model: window.model })
			: window.label;

	return (
		<div className="flex flex-col gap-3">
			{troubled.length > 0 && !unavailable ? (
				<p className="text-xs text-muted-foreground" data-fleet-host-states="true">
					{troubled.map((host) => t("usage.fleet.hostState", { host: hostName(host), state: stateText(host.state) })).join(" · ")}
				</p>
			) : null}
			{unavailable ? (
				<div className="flex flex-col items-center gap-2 py-16 text-center" data-fleet-unavailable={unavailable.hostId}>
					<p className="text-control font-medium text-foreground">{t("usage.fleet.hostState", { host: hostName(unavailable), state: stateText(unavailable.state) })}</p>
					<p className="max-w-sm text-xs text-muted-foreground">{unavailableBody(unavailable.state)}</p>
				</div>
			) : rows.length === 0 ? (
				<div className="flex flex-col items-center gap-2 py-16 text-center">
					<p className="text-control font-medium text-foreground">{t("usage.limits.empty")}</p>
					<p className="max-w-sm text-xs text-muted-foreground">{t("usage.limits.emptyBody")}</p>
					<Button variant="ghost" size="sm" onClick={() => openGlobalSettings("harness")}>{t("usage.limits.openSettings")}</Button>
				</div>
			) : (
				<Table aria-label={t("usage.fleet.table")} className="text-control">
					<TableHeader>
						<TableRow className="hover:bg-transparent">
							<TableHead className="ps-0 font-normal text-muted-foreground">{t("usage.fleet.column.provider")}</TableHead>
							<TableHead className="font-normal text-muted-foreground">{t("usage.fleet.column.account")}</TableHead>
							<TableHead className="font-normal text-muted-foreground">{t("usage.fleet.column.plan")}</TableHead>
							<TableHead className="font-normal text-muted-foreground">{t("usage.fleet.column.window")}</TableHead>
							<TableHead className="text-end font-normal text-muted-foreground">{t("usage.fleet.column.used")}</TableHead>
							<TableHead className="font-normal text-muted-foreground">{t("usage.fleet.column.resetsAt")}</TableHead>
							<TableHead className="font-normal text-muted-foreground">{t("usage.fleet.column.read")}</TableHead>
							{showHosts ? <TableHead className="pe-0 font-normal text-muted-foreground">{t("usage.fleet.column.hosts")}</TableHead> : null}
						</TableRow>
					</TableHeader>
					<TableBody>
						{rows.map((row) => {
							const problem = describeAccountProblem(row.account, t)?.text ?? (row.account.signedIn ? null : t("usage.limits.signedOut"));
							const plan = formatPlanName(row.plan);
							return (
								<TableRow key={row.key} className="hover:bg-transparent" data-fleet-account={row.accountKey} data-fleet-window={row.window?.id ?? ""}>
									<TableCell className="ps-0">
										<span className="inline-flex items-center gap-2">
											<AgentAvatar provider={PROVIDER_HARNESS[row.provider]} decorative className="size-4" />
											{agentAccountProviderNames[row.provider]}
										</span>
									</TableCell>
									<TableCell className="max-w-64">
										<span className="block truncate text-foreground">{row.email ?? agentAccountName(row.account, t)}</span>
										{problem ? <span className="block truncate text-xs text-muted-foreground">{problem}</span> : null}
									</TableCell>
									<TableCell className="text-muted-foreground">{plan ?? "—"}</TableCell>
									<TableCell>{row.window ? windowName(row.window) : <span className="text-muted-foreground">{t("usage.limits.noWindows")}</span>}</TableCell>
									<TableCell className="text-end tabular-nums">
										{row.resetPassed ? <span className="text-xs text-muted-foreground">{t("usage.fleet.resetAwaiting")}</span>
											: row.usedPercent === null ? <span className="text-muted-foreground">—</span>
											: percent.format(row.usedPercent / 100)}
									</TableCell>
									<TableCell className="tabular-nums text-muted-foreground">
										{row.resetsAt && !row.resetPassed ? <time dateTime={row.resetsAt}>{instant.format(new Date(row.resetsAt))}</time> : "—"}
									</TableCell>
									<TableCell className="tabular-nums text-muted-foreground">
										{row.readAt ? (
											<time dateTime={row.readAt} title={absolute.format(new Date(row.readAt))}>
												{showHosts && row.readOn
													? t("usage.fleet.readOn", { age: formatRelativeTime(row.readAt, now, i18n.language), host: hostName(row.readOn) })
													: formatRelativeTime(row.readAt, now, i18n.language)}
											</time>
										) : "—"}
									</TableCell>
									{showHosts ? <TableCell className="pe-0 text-muted-foreground">{row.hosts.map(hostName).join(" · ")}</TableCell> : null}
								</TableRow>
							);
						})}
					</TableBody>
				</Table>
			)}
		</div>
	);
}
