package cli

import (
	"context"
	"fmt"
	"io"
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

// usageLimitsSource reads one daemon's limit routes. The local daemon is the
// only source today; a later --fleet can add one per paired remote host and
// merge their answers, without changing how a single answer is rendered.
type usageLimitsSource func(ctx context.Context, path string, out any) error

const maxUsageLimitWeeks = 26

func newUsageCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Read subscription limit usage",
	}
	cmd.AddCommand(newUsageLimitsCommand(ctx))
	return cmd
}

func newUsageLimitsCommand(ctx *commandContext) *cobra.Command {
	var history, asJSON bool
	var weeks int
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
			source := usageLimitsSource(ctx.getJSON)
			if history {
				var res usageLimitPeaksResponse
				if err := source(cmd.Context(), "usage/limit-peaks?weeks="+strconv.Itoa(weeks), &res); err != nil {
					return err
				}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				return writeUsageLimitPeaks(cmd.OutOrStdout(), res)
			}
			var res usageLimitAccountsResponse
			if err := source(cmd.Context(), "agents/accounts", &res); err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			return writeUsageLimits(cmd.OutOrStdout(), res.Accounts)
		},
	}
	cmd.Flags().BoolVar(&history, "history", false, "Show the peak each window reached per reset period instead of the current level")
	cmd.Flags().IntVar(&weeks, "weeks", 12, "With --history, the weeks of reset periods to show (1-26)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	return cmd
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
