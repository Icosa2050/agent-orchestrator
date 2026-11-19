import { useTranslation } from "react-i18next";
import { agentAccountProviderNames } from "../../hooks/useAgentAccounts";
import type { FleetHostRef } from "../../lib/fleet-limits";
import type { PeakHistoryRow } from "../../lib/limit-peaks";
import { PROVIDER_HARNESS } from "../../lib/usage-limits";
import { AgentAvatar } from "../AgentAvatar";
import { formatPlanName } from "../settings/AgentAccountsSection";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";

/** How many of a window's latest periods the table lists; the CLI's --json has them all. */
export const PEAK_PERIODS_SHOWN = 6;

/**
 * Limit history as raw numbers: per account and window, the peak each recent
 * reset period reached and a summary of the periods that have reset. No chart.
 */
export function UsageLimitPeaks({ rows, weeks, showHosts }: { rows: readonly PeakHistoryRow[]; weeks: number; showHosts: boolean }) {
	const { t, i18n } = useTranslation();
	const percent = new Intl.NumberFormat(i18n.language, { style: "percent", maximumFractionDigits: 0 });
	const day = new Intl.DateTimeFormat(i18n.language, { day: "numeric", month: "short" });
	const absolute = new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short" });
	const pct = (value: number) => percent.format(value / 100);
	const hostName = (host: FleetHostRef) => host.label ?? t("remote.thisComputer");
	const windowName = (window: PeakHistoryRow["window"]) =>
		window.kind === "5h" ? t("usage.fleet.window.fiveHour")
			: window.kind === "weekly" ? t("usage.fleet.window.weekly")
			: window.kind === "weekly-model" && window.model ? t("usage.fleet.window.weeklyModel", { model: window.model })
			: window.label;
	if (rows.length === 0) return null;

	return (
		<section className="mt-8 flex flex-col gap-2" aria-labelledby="usage-peaks-title">
			<div className="flex flex-col gap-0.5">
				<h2 id="usage-peaks-title" className="text-control font-medium text-foreground">{t("usage.peaks.title")}</h2>
				<p className="text-xs text-muted-foreground">{t("usage.peaks.about", { weeks })}</p>
			</div>
			<Table aria-label={t("usage.peaks.table")} className="text-control">
				<TableHeader>
					<TableRow className="hover:bg-transparent">
						<TableHead className="ps-0 font-normal text-muted-foreground">{t("usage.peaks.column.account")}</TableHead>
						<TableHead className="font-normal text-muted-foreground">{t("usage.peaks.column.window")}</TableHead>
						<TableHead className="font-normal text-muted-foreground">{t("usage.peaks.column.peaks")}</TableHead>
						<TableHead className="text-end font-normal text-muted-foreground">{t("usage.peaks.column.periods")}</TableHead>
						<TableHead className="text-end font-normal text-muted-foreground">{t("usage.peaks.column.avg")}</TableHead>
						<TableHead className="text-end font-normal text-muted-foreground">{t("usage.peaks.column.max")}</TableHead>
						<TableHead className="text-end font-normal text-muted-foreground">{t("usage.peaks.column.over80")}</TableHead>
						<TableHead className={showHosts ? "text-end font-normal text-muted-foreground" : "pe-0 text-end font-normal text-muted-foreground"}>{t("usage.peaks.column.under30")}</TableHead>
						{showHosts ? <TableHead className="pe-0 font-normal text-muted-foreground">{t("usage.fleet.column.hosts")}</TableHead> : null}
					</TableRow>
				</TableHeader>
				<TableBody>
					{rows.map((row) => {
						const plan = formatPlanName(row.plan);
						const closed = row.summary.periods > 0;
						return (
							<TableRow key={row.key} className="hover:bg-transparent" data-peak-row={row.key}>
								<TableCell className="ps-0 max-w-64">
									<span className="inline-flex max-w-full items-center gap-2">
										<AgentAvatar provider={PROVIDER_HARNESS[row.provider]} decorative className="size-4 shrink-0" />
										<span className="truncate text-foreground">{row.email ?? agentAccountProviderNames[row.provider]}</span>
									</span>
									{plan ? <span className="block truncate ps-6 text-xs text-muted-foreground">{plan}</span> : null}
								</TableCell>
								<TableCell>{windowName(row.window)}</TableCell>
								<TableCell className="tabular-nums">
									<span className="flex flex-wrap gap-x-3 gap-y-0.5">
										{row.periods.slice(-PEAK_PERIODS_SHOWN).map((period) => (
											<span
												key={period.resetAt}
												data-peak-period={period.resetAt}
												title={[t("usage.peaks.periodTitle", { reset: absolute.format(new Date(period.resetAt)), readings: period.readings }), period.open ? t("usage.peaks.open") : null].filter(Boolean).join(" · ")}
												className={period.open ? "text-muted-foreground" : undefined}
											>
												<span className="text-muted-foreground">{day.format(new Date(period.resetAt))}</span>{" "}
												{pct(period.peakUsedPct)}
												{period.open ? <span className="sr-only"> ({t("usage.peaks.open")})</span> : null}
												{period.open ? <span aria-hidden="true">*</span> : null}
											</span>
										))}
									</span>
								</TableCell>
								<TableCell className="text-end tabular-nums">{row.summary.periods}</TableCell>
								<TableCell className="text-end tabular-nums">{closed ? pct(row.summary.avgPeak) : "—"}</TableCell>
								<TableCell className="text-end tabular-nums">{closed ? pct(row.summary.maxPeak) : "—"}</TableCell>
								<TableCell className="text-end tabular-nums">{closed ? row.summary.periodsOver80 : "—"}</TableCell>
								<TableCell className={showHosts ? "text-end tabular-nums" : "pe-0 text-end tabular-nums"}>{closed ? row.summary.periodsUnder30 : "—"}</TableCell>
								{showHosts ? <TableCell className="pe-0 text-muted-foreground">{row.hosts.map(hostName).join(" · ")}</TableCell> : null}
							</TableRow>
						);
					})}
				</TableBody>
			</Table>
		</section>
	);
}
