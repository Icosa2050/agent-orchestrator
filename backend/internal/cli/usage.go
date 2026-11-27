package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type usageLimitWindow struct {
	ID              string     `json:"id"`
	Label           string     `json:"label"`
	UsedPercent     float64    `json:"usedPercent"`
	ResetsAt        *time.Time `json:"resetsAt,omitempty"`
	DurationMinutes int        `json:"durationMinutes,omitempty"`
}

type usageLimitAccount struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Email    string `json:"email,omitempty"`
	Plan     string `json:"plan,omitempty"`
	SignedIn bool   `json:"signedIn"`
	Limits   *struct {
		Windows   []usageLimitWindow `json:"windows"`
		FetchedAt time.Time          `json:"fetchedAt"`
	} `json:"limits,omitempty"`
}

type usageLimitAccountsResponse struct {
	Accounts []usageLimitAccount `json:"accounts"`
}

type usageLimitPeak struct {
	ResetAt        time.Time `json:"resetAt"`
	PeakUsedPct    float64   `json:"peakUsedPct"`
	Readings       int       `json:"readings"`
	FirstReadingAt time.Time `json:"firstReadingAt"`
	LastReadingAt  time.Time `json:"lastReadingAt"`
	Open           bool      `json:"open"`
}

type usageLimitDayPeak struct {
	Day        string  `json:"day"`
	MaxUsedPct float64 `json:"maxUsedPct"`
	Readings   int     `json:"readings"`
}

type usageLimitPeakSummary struct {
	Periods        int     `json:"periods"`
	AvgPeak        float64 `json:"avgPeak"`
	MaxPeak        float64 `json:"maxPeak"`
	PeriodsOver80  int     `json:"periodsOver80"`
	PeriodsUnder30 int     `json:"periodsUnder30"`
}

type usageLimitPeakTrack struct {
	Provider        string                `json:"provider"`
	Login           string                `json:"login"`
	Email           string                `json:"email,omitempty"`
	AccountID       string                `json:"accountId"`
	Plan            string                `json:"plan,omitempty"`
	WindowID        string                `json:"windowId"`
	Label           string                `json:"label"`
	DurationMinutes int                   `json:"durationMinutes,omitempty"`
	Periods         []usageLimitPeak      `json:"periods"`
	Days            []usageLimitDayPeak   `json:"days"`
	Summary         usageLimitPeakSummary `json:"summary"`
}

type usageLimitPeaksResponse struct {
	Since   time.Time             `json:"since"`
	Until   time.Time             `json:"until"`
	Windows []usageLimitPeakTrack `json:"windows"`
}

const (
	maxUsageLimitWeeks = 26
	maxUsageLimitDays  = 14
)

// newUsageCommand is the single `ao usage` tree. Every subcommand only issues
// GETs, so each opts into --host and also takes --fleet.
func newUsageCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Read token usage and subscription limit windows",
	}
	cmd.AddCommand(markRemoteReadOnly(newUsageReportCommand(ctx)), markRemoteReadOnly(newUsageLimitsCommand(ctx)))
	// Fork: usage seam (FORK.md). `prices` writes (check, accept, auto), so it stays local; `window` only reads.
	cmd.AddCommand(newForkUsagePricesCommand(ctx), markRemoteReadOnly(newForkUsageWindowCommand(ctx)))
	// Fork: usage seam (FORK.md). `providers status` reads; `providers enable|disable` write and stay local.
	cmd.AddCommand(newForkUsageProvidersCommand(ctx))
	return cmd
}

func newUsageReportCommand(ctx *commandContext) *cobra.Command {
	var days, offset, top int
	var tz, project, by string
	var fleet, asJSON bool
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Print token and estimated cost usage over a window as JSON",
		Long: "Print token and estimated cost usage over a window as JSON.\n\n" +
			"By default rows are per period, harness, model and speed tier. --by session reads one row per session, harness,\n" +
			"model and speed tier over the whole window, under \"sessionRows\" with the session and project id, costliest first.\n" +
			"--top N keeps only the N costliest rows.\n\n" + forkUsageReportHelp, // Fork: usage seam (FORK.md)
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if by == "hour" { // Fork: usage seam (FORK.md): DeepSeek hour-of-day histogram.
				if cmd.Flags().Changed("top") {
					return usageError{fmt.Errorf("--top does not apply to --by hour")}
				}
				return runForkUsageReportHours(cmd, ctx, forkHoursArgs{days: days, offset: offset, tz: tz, project: project, fleet: fleet, json: asJSON})
			}
			if by != "" && by != "period" && by != "session" {
				return usageError{fmt.Errorf("--by must be period or session, got %q", by)}
			}
			if cmd.Flags().Changed("top") && top < 1 {
				return usageError{fmt.Errorf("--top must be at least 1, got %d", top)}
			}
			params := url.Values{}
			if by == "session" {
				params.Set("groupBy", by)
			}
			if top > 0 {
				params.Set("top", strconv.Itoa(top))
			}
			if days > 0 {
				params.Set("days", strconv.Itoa(days))
			}
			if offset > 0 {
				params.Set("offset", strconv.Itoa(offset))
			}
			if tz != "" {
				params.Set("tz", tz)
			}
			if project != "" {
				params.Set("projectId", project)
			}
			path := apiPath("usage/report", params)
			if fleet {
				hosts, err := ctx.fleetGetJSON(cmd.Context(), path)
				if err != nil {
					return err
				}
				for i := range hosts {
					if hosts[i].Status == remoteStatusOK {
						if err := checkReportAnswer(hosts[i].Data, by, top); err != nil {
							hosts[i].Status, hosts[i].Error, hosts[i].Data = remoteStatusError, err.Error(), nil
						}
					}
				}
				// Fork: usage seam (FORK.md): each host's DeepSeek report extension.
				if err := forkMergeFleetUsageReport(cmd.Context(), ctx, hosts, params, by, top); err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), map[string]any{"hosts": hosts})
			}
			var res json.RawMessage
			if err := ctx.getJSON(cmd.Context(), path, &res); err != nil {
				return err
			}
			if err := checkReportAnswer(res, by, top); err != nil {
				return err
			}
			// Fork: usage seam (FORK.md): merge the DeepSeek report extension.
			res, err := forkMergeUsageReport(cmd.Context(), ctx, res, params, by, top)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().IntVar(&days, "days", 0, "Days to read, today included (1-90; default 7)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Whole windows to step back")
	cmd.Flags().StringVar(&tz, "tz", "", "IANA time zone for day buckets (default UTC)")
	cmd.Flags().StringVarP(&project, "project", "p", "", "Read one project only")
	cmd.Flags().StringVar(&by, "by", "", "Group rows by period (default), session, or hour (DeepSeek, UTC hour of day)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "With --by hour, output JSON (the other groupings always print JSON)") // Fork: usage seam (FORK.md)
	cmd.Flags().IntVar(&top, "top", 0, "Keep only the N costliest rows (default: all)")
	cmd.Flags().BoolVar(&fleet, "fleet", false, "Read this machine and every saved remote (ao remote ls); returns {\"hosts\": [...]}")
	return cmd
}

// checkReportAnswer catches a daemon that predates --by session or --top: it ignores the parameter and answers
// with period rows in full, which would pass for the asked-for answer. Current daemons echo what they applied.
func checkReportAnswer(data json.RawMessage, by string, top int) error {
	var probe struct {
		GroupBy string `json:"groupBy"`
		Top     int    `json:"top"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("decode usage report: %w", err)
	}
	if by == "session" && probe.GroupBy != "session" {
		return errors.New("this daemon does not support --by session; update it")
	}
	if top > 0 && probe.Top != top {
		return errors.New("this daemon does not support --top; update it")
	}
	return nil
}

// usageLimitsMode is the one limit route a `usage limits` run reads.
type usageLimitsMode int

const (
	usageLimitsCurrent usageLimitsMode = iota
	usageLimitsHistory
	usageLimitsReadings
)

func newUsageLimitsCommand(ctx *commandContext) *cobra.Command {
	var history, asJSON, fleet bool
	var weeks, days int
	cmd := &cobra.Command{
		Use:   "limits",
		Short: "Show each account's limit windows now, or with --history the peak each window reached per reset period",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if weeks < 1 || weeks > maxUsageLimitWeeks {
				return usageError{fmt.Errorf("--weeks must be between 1 and %d, got %d", maxUsageLimitWeeks, weeks)}
			}
			if cmd.Flags().Changed("weeks") && !history {
				return usageError{fmt.Errorf("--weeks needs --history")}
			}
			mode, path := usageLimitsCurrent, "agents/accounts"
			switch {
			case cmd.Flags().Changed("days"):
				if history {
					return usageError{fmt.Errorf("--days reads raw limit-window readings; do not combine it with --history")}
				}
				if days < 1 || days > maxUsageLimitDays {
					return usageError{fmt.Errorf("--days must be between 1 and %d, got %d", maxUsageLimitDays, days)}
				}
				mode, path = usageLimitsReadings, "usage/limit-windows?days="+strconv.Itoa(days)
			case history:
				mode, path = usageLimitsHistory, "usage/limit-peaks?weeks="+strconv.Itoa(weeks)
			}
			if fleet {
				hosts, err := ctx.fleetGetJSON(cmd.Context(), path)
				if err != nil {
					return err
				}
				if mode == usageLimitsCurrent { // Fork: usage seam (FORK.md): provider rows, one per key.
					return writeForkFleetUsageLimits(cmd.Context(), ctx, cmd.OutOrStdout(), hosts, asJSON)
				}
				if asJSON || mode == usageLimitsReadings {
					return writeJSON(cmd.OutOrStdout(), map[string]any{"hosts": hosts})
				}
				return writeFleetUsageLimits(cmd.OutOrStdout(), hosts, mode)
			}
			var res json.RawMessage
			if err := ctx.getJSON(cmd.Context(), path, &res); err != nil {
				return err
			}
			if mode == usageLimitsCurrent { // Fork: usage seam (FORK.md): provider rows and the DeepSeek hint.
				return writeForkUsageLimits(cmd.Context(), ctx, cmd.OutOrStdout(), res, asJSON)
			}
			if asJSON || mode == usageLimitsReadings {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writeUsageLimitsData(cmd.OutOrStdout(), res, mode)
		},
	}
	cmd.Flags().BoolVar(&history, "history", false, "Show the peak each window reached per reset period instead of the current level")
	cmd.Flags().IntVar(&weeks, "weeks", 12, "With --history, the weeks of reset periods to show (1-26)")
	cmd.Flags().IntVar(&days, "days", 0, "Print the raw five-hour and weekly limit-window readings of the last N days as JSON (1-14)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	cmd.Flags().BoolVar(&fleet, "fleet", false, "Read this machine and every saved remote (ao remote ls); --json returns {\"hosts\": [...]}")
	return cmd
}

// writeUsageLimitsData renders one host's answer for the current or history mode.
func writeUsageLimitsData(out io.Writer, data json.RawMessage, mode usageLimitsMode) error {
	if mode == usageLimitsHistory {
		var res usageLimitPeaksResponse
		if err := json.Unmarshal(data, &res); err != nil {
			return fmt.Errorf("decode limit history: %w", err)
		}
		return writeUsageLimitPeaks(out, res)
	}
	var res usageLimitAccountsResponse
	if err := json.Unmarshal(data, &res); err != nil {
		return fmt.Errorf("decode accounts: %w", err)
	}
	return writeUsageLimits(out, res.Accounts)
}

func writeFleetUsageLimits(out io.Writer, hosts []fleetUsageHost, mode usageLimitsMode) error {
	for i, h := range hosts {
		if i > 0 {
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		label := h.Name
		if !h.Local {
			label += " " + h.URL
		}
		if _, err := fmt.Fprintf(out, "== %s: %s ==\n", label, h.Status); err != nil {
			return err
		}
		if h.Status != remoteStatusOK {
			if _, err := fmt.Fprintf(out, "  %s\n", orDash(h.Error)); err != nil {
				return err
			}
			continue
		}
		if err := writeUsageLimitsData(out, h.Data, mode); err != nil {
			return err
		}
	}
	return nil
}

func usageAccountName(provider, email, fallback string) string {
	if email == "" {
		email = fallback
	}
	return provider + " " + email
}

func writeUsageLimits(out io.Writer, accounts []usageLimitAccount) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tPLAN\tWINDOW\tUSED\tRESETS\tREAD")
	for _, account := range accounts {
		name := usageAccountName(account.Provider, account.Email, account.ID)
		if account.Limits == nil || len(account.Limits.Windows) == 0 {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\n", name, dash(account.Plan))
			continue
		}
		for _, w := range account.Limits.Windows {
			resets := "-"
			if w.ResetsAt != nil {
				resets = w.ResetsAt.Local().Format("Mon 02 Jan 15:04")
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f%%\t%s\t%s\n", name, dash(account.Plan), dash(w.Label), w.UsedPercent, resets,
				account.Limits.FetchedAt.Local().Format("15:04"))
		}
	}
	return tw.Flush()
}

// usageHistoryColumns is how many of the latest periods the table shows; --json has them all.
const usageHistoryColumns = 6

func writeUsageLimitPeaks(out io.Writer, res usageLimitPeaksResponse) error {
	if len(res.Windows) == 0 {
		_, err := fmt.Fprintln(out, "No limit history yet. AO keeps it while limit history is turned on.")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tWINDOW\tPERIODS\tAVG\tMAX\t>=80%\t<30%\tLATEST PEAKS (oldest first, * still open)")
	for _, track := range res.Windows {
		recent := track.Periods
		if len(recent) > usageHistoryColumns {
			recent = recent[len(recent)-usageHistoryColumns:]
		}
		peaks := make([]string, 0, len(recent))
		for _, p := range recent {
			cell := fmt.Sprintf("%s %.0f%%", p.ResetAt.Local().Format("02 Jan"), p.PeakUsedPct)
			if p.Open {
				cell += "*"
			}
			peaks = append(peaks, cell)
		}
		avg, peak := "-", "-"
		if track.Summary.Periods > 0 {
			avg, peak = fmt.Sprintf("%.0f%%", track.Summary.AvgPeak), fmt.Sprintf("%.0f%%", track.Summary.MaxPeak)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%d\t%d\t%s\n", usageAccountName(track.Provider, track.Email, track.Login), dash(track.Label),
			track.Summary.Periods, avg, peak, track.Summary.PeriodsOver80, track.Summary.PeriodsUnder30, strings.Join(peaks, ", "))
	}
	return tw.Flush()
}
