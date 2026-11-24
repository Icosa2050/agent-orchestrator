package cli

// Fork-only (Icosa2050, see FORK.md "Usage seams"): `ao usage prices` and
// `ao usage window`, thin clients of the daemon's /api/v1/usage/fork routes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Exit codes of `ao usage prices --check`.
const (
	exitPricesChanged      = 3
	exitPricesCannotVerify = 4
)

// codedError carries its own process exit code (see ExitCode).
type codedError struct {
	code int
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }
func (e codedError) ExitCode() int { return e.code }

type forkRates struct {
	CacheHit  string `json:"cacheHit"`
	CacheMiss string `json:"cacheMiss"`
	Output    string `json:"output"`
}

type forkModelRates struct {
	Model         string    `json:"model"`
	PeakPerM      forkRates `json:"peakPerM"`
	OffPeakPerM   forkRates `json:"offPeakPerM"`
	RetiredOnPage bool      `json:"retiredOnPage"`
}

type forkPriceVersion struct {
	Version       string    `json:"version"`
	EffectiveFrom time.Time `json:"effectiveFrom"`
	Source        string    `json:"source"`
	Label         string    `json:"label"`
}

type forkPriceStatus struct {
	Provider        string             `json:"provider"`
	Currency        string             `json:"currency"`
	Version         string             `json:"version"`
	Source          string             `json:"source"`
	Label           string             `json:"label"`
	EffectiveFrom   time.Time          `json:"effectiveFrom"`
	AutoCheck       bool               `json:"autoCheck"`
	LastCheckAt     *time.Time         `json:"lastCheckAt"`
	LastCheckStatus string             `json:"lastCheckStatus"`
	LastCheckReason string             `json:"lastCheckReason"`
	LastSuccessAt   *time.Time         `json:"lastSuccessAt"`
	Stale           bool               `json:"stale"`
	Warnings        []string           `json:"warnings"`
	WindowRule      string             `json:"windowRule"`
	Rates           []forkModelRates   `json:"rates"`
	Versions        []forkPriceVersion `json:"versions"`
	HolidayYears    []struct {
		Year int `json:"year"`
	} `json:"holidayYears"`
}

type forkCheckResult struct {
	Outcome    string          `json:"outcome"`
	Reason     string          `json:"reason"`
	Recorded   string          `json:"recordedVersion"`
	PageSHA256 string          `json:"pageSha256"`
	Diff       json.RawMessage `json:"diff"`
	Status     forkPriceStatus `json:"status"`
}

func newForkUsagePricesCommand(ctx *commandContext) *cobra.Command {
	var check, asJSON bool
	var accept string
	cmd := &cobra.Command{
		Use:   "prices [deepseek]",
		Short: "Show the DeepSeek price book; --check compares it with DeepSeek's pricing page now",
		Long: "Show the DeepSeek price book: the version in effect, its rates (USD per 1M tokens, peak and off-peak),\n" +
			"the automatic daily price-page check and any warnings.\n\n" +
			"--check fetches https://api-docs.deepseek.com/quick_start/pricing.md now (keyless). Exit codes: 0 unchanged,\n" +
			"3 changed (recorded as a new version, or held as suspicious until --accept), 4 cannot verify.\n" +
			"--accept <pageSha256> records a held suspicious change. `ao usage prices auto on|off` switches the daily check.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] != "deepseek" {
				return usageError{fmt.Errorf("unknown provider %q; only deepseek has a price book", args[0])}
			}
			if check && accept != "" {
				return usageError{errors.New("use either --check or --accept")}
			}
			out := cmd.OutOrStdout()
			switch {
			case check || accept != "":
				var res forkCheckResult
				var err error
				if check {
					err = ctx.postJSON(cmd.Context(), "usage/fork/prices/check", map[string]any{}, &res)
				} else {
					err = ctx.postJSON(cmd.Context(), "usage/fork/prices/accept", map[string]string{"pageSha256": accept}, &res)
				}
				if err != nil {
					return forkUsageRouteError(err)
				}
				if asJSON {
					if err := writeJSON(out, res); err != nil {
						return err
					}
				} else if err := writeForkCheck(out, res); err != nil {
					return err
				}
				switch res.Outcome {
				case "changed", "suspicious":
					return codedError{exitPricesChanged, fmt.Errorf("deepseek prices %s", res.Outcome)}
				case "cannot-verify":
					return codedError{exitPricesCannotVerify, fmt.Errorf("deepseek prices: cannot verify: %s", res.Reason)}
				}
				return nil
			default:
				var raw json.RawMessage
				if err := ctx.getJSON(cmd.Context(), "usage/fork/prices", &raw); err != nil {
					return forkUsageRouteError(err)
				}
				if asJSON {
					return writeJSON(out, raw)
				}
				var st forkPriceStatus
				if err := json.Unmarshal(raw, &st); err != nil {
					return fmt.Errorf("decode price book: %w", err)
				}
				return writeForkPriceStatus(out, st, time.Now())
			}
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "Check DeepSeek's pricing page now")
	cmd.Flags().StringVar(&accept, "accept", "", "Record the held suspicious change with this page SHA-256")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	cmd.AddCommand(newForkUsagePricesAutoCommand(ctx))
	return cmd
}

func newForkUsagePricesAutoCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:       "auto on|off",
		Short:     "Switch the automatic daily DeepSeek price-page check (default on)",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "on" && args[0] != "off" {
				return usageError{fmt.Errorf("want on or off, got %q", args[0])}
			}
			var st forkPriceStatus
			if err := ctx.postJSON(cmd.Context(), "usage/fork/prices/auto", map[string]bool{"enabled": args[0] == "on"}, &st); err != nil {
				return forkUsageRouteError(err)
			}
			state := "off"
			if st.AutoCheck {
				state = "on"
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "automatic DeepSeek price check: %s\n", state)
			return err
		},
	}
}

type forkTimePair struct {
	UTC    time.Time `json:"utc"`
	Berlin string    `json:"berlin"`
}

type forkSpanPair struct {
	From  forkTimePair `json:"from"`
	Until forkTimePair `json:"until"`
}

type forkWindowReport struct {
	At              forkTimePair  `json:"at"`
	Window          string        `json:"window"`
	PriceFactor     string        `json:"priceFactor"`
	PeakVsOffPeak   string        `json:"peakVsOffPeak"`
	Reason          string        `json:"reason"`
	Holiday         string        `json:"holiday"`
	WindowEndsAt    *forkTimePair `json:"windowEndsAt"`
	NextOffPeak     *forkSpanPair `json:"nextOffPeak"`
	NextPeak        *forkSpanPair `json:"nextPeak"`
	WindowRule      string        `json:"windowRule"`
	HolidayCalendar string        `json:"holidayCalendar"`
	Assumptions     []string      `json:"assumptions"`
	NoPriceEvidence bool          `json:"noPriceEvidence"`
	PriceBook       struct {
		Version  string   `json:"version"`
		Stale    bool     `json:"stale"`
		Warnings []string `json:"warnings"`
	} `json:"priceBook"`
}

func newForkUsageWindowCommand(ctx *commandContext) *cobra.Command {
	var at, next string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "window [deepseek]",
		Short: "Show DeepSeek's peak/off-peak price window now (or --at), and the next cheap window in UTC and Berlin time",
		Long: "Show DeepSeek's peak/off-peak price window: off-peak is half price. Peak is 01:00-04:00 and 06:00-10:00 UTC,\n" +
			"Monday to Friday, except Chinese public holidays. Computed locally from the price book; no network.\n\n" +
			"--next offpeak|peak prints only the RFC 3339 start of the current or next such window (now when it already is),\n" +
			"for scripts and timers. --at evaluates another instant (RFC 3339).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] != "deepseek" {
				return usageError{fmt.Errorf("unknown provider %q; only deepseek has price windows", args[0])}
			}
			next = strings.ReplaceAll(strings.ToLower(next), "-", "")
			if next != "" && next != "offpeak" && next != "peak" {
				return usageError{fmt.Errorf("--next must be offpeak or peak, got %q", next)}
			}
			params := url.Values{}
			if at != "" && at != "now" {
				parsed, err := time.Parse(time.RFC3339Nano, at)
				if err != nil {
					return usageError{fmt.Errorf("--at must be an RFC 3339 time such as 2026-11-25T09:30:00Z: %w", err)}
				}
				params.Set("at", parsed.UTC().Format(time.RFC3339Nano))
			}
			var raw json.RawMessage
			if err := ctx.getJSON(cmd.Context(), apiPath("usage/fork/window", params), &raw); err != nil {
				return forkUsageRouteError(err)
			}
			var rep forkWindowReport
			if err := json.Unmarshal(raw, &rep); err != nil {
				return fmt.Errorf("decode window: %w", err)
			}
			out := cmd.OutOrStdout()
			if next != "" {
				start, err := forkNextStart(rep, next == "peak")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(out, start.UTC().Format(time.RFC3339))
				return err
			}
			if asJSON {
				return writeJSON(out, raw)
			}
			return writeForkWindow(out, rep)
		},
	}
	cmd.Flags().StringVar(&at, "at", "", "Evaluate this instant (RFC 3339) instead of now")
	cmd.Flags().StringVar(&next, "next", "", "Print only the start of the current or next offpeak|peak window")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	return cmd
}

func forkNextStart(rep forkWindowReport, peak bool) (time.Time, error) {
	want := "off-peak"
	if peak {
		want = "peak"
	}
	if rep.Window == want {
		return rep.At.UTC, nil
	}
	span := rep.NextOffPeak
	if peak {
		span = rep.NextPeak
	}
	if span == nil {
		return time.Time{}, fmt.Errorf("no %s window within the next 40 days", want)
	}
	return span.From.UTC, nil
}

// forkUsageRouteError explains a 404 from a daemon that predates the routes.
func forkUsageRouteError(err error) error {
	var apiErr apiResponseError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return errors.New("this daemon has no DeepSeek price book; update it")
	}
	return err
}

func forkClock(t time.Time) string {
	return t.UTC().Format("Mon 02 Jan 15:04") + " UTC"
}

func forkBerlinClock(p forkTimePair) string {
	b, err := time.Parse(time.RFC3339, p.Berlin)
	if err != nil {
		return forkClock(p.UTC)
	}
	return fmt.Sprintf("%s (%s Berlin)", forkClock(p.UTC), b.Format("15:04"))
}

func writeForkWindow(out io.Writer, rep forkWindowReport) error {
	var b strings.Builder
	label := strings.ToUpper(rep.Window)
	price := "full price"
	if rep.Window == "off-peak" {
		price = "price x" + rep.PriceFactor
	} else if rep.PeakVsOffPeak != "" {
		price = rep.PeakVsOffPeak + "x the off-peak price"
	}
	fmt.Fprintf(&b, "deepseek: %s at %s (%s; %s)\n", label, forkBerlinClock(rep.At), rep.Reason, price)
	if rep.WindowEndsAt != nil {
		fmt.Fprintf(&b, "  until          %s\n", forkBerlinClock(*rep.WindowEndsAt))
	}
	for _, s := range []struct {
		name string
		span *forkSpanPair
	}{{"next off-peak", rep.NextOffPeak}, {"next peak", rep.NextPeak}} {
		if s.span != nil {
			fmt.Fprintf(&b, "  %-14s %s -> %s\n", s.name, forkBerlinClock(s.span.From), forkBerlinClock(s.span.Until))
		}
	}
	fmt.Fprintf(&b, "  holidays: %s; prices: %s\n", rep.HolidayCalendar, rep.PriceBook.Version)
	fmt.Fprintf(&b, "  rule: %s\n", rep.WindowRule)
	if rep.NoPriceEvidence {
		b.WriteString("  note: before the first evidenced price version; events then are not priced\n")
	}
	for _, a := range rep.Assumptions {
		fmt.Fprintf(&b, "  assumption: %s\n", a)
	}
	for _, w := range rep.PriceBook.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func forkAge(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func writeForkPriceStatus(out io.Writer, st forkPriceStatus, now time.Time) error {
	var b strings.Builder
	source := st.Source
	if st.Label != "" {
		source = st.Label
	}
	fmt.Fprintf(&b, "prices: %s (%s; effective %s)\n", st.Version, source, forkClock(st.EffectiveFrom))
	auto := "off"
	if st.AutoCheck {
		auto = "on (daily)"
	}
	last := "never"
	if st.LastCheckAt != nil {
		last = fmt.Sprintf("%s, %s", forkAge(now, *st.LastCheckAt), orDash(st.LastCheckStatus))
	}
	fmt.Fprintf(&b, "check: auto %s; last check %s\n", auto, last)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "MODEL\tWINDOW\tCACHE HIT\tCACHE MISS\tOUTPUT\t(%s per 1M tokens)\n", st.Currency)
	for _, r := range st.Rates {
		name := r.Model
		if r.RetiredOnPage {
			name += " (no longer listed)"
		}
		_, _ = fmt.Fprintf(tw, "%s\tpeak\t%s\t%s\t%s\t\n", name, r.PeakPerM.CacheHit, r.PeakPerM.CacheMiss, r.PeakPerM.Output)
		_, _ = fmt.Fprintf(tw, "\toff-peak\t%s\t%s\t%s\t\n", r.OffPeakPerM.CacheHit, r.OffPeakPerM.CacheMiss, r.OffPeakPerM.Output)
	}
	_ = tw.Flush()
	years := make([]string, 0, len(st.HolidayYears))
	for _, y := range st.HolidayYears {
		years = append(years, fmt.Sprint(y.Year))
	}
	fmt.Fprintf(&b, "versions: %d; holidays: %s\n", len(st.Versions), strings.Join(years, ", "))
	fmt.Fprintf(&b, "window rule: %s\n", st.WindowRule)
	for _, w := range st.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func writeForkCheck(out io.Writer, res forkCheckResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "check: %s", res.Outcome)
	if res.Reason != "" {
		fmt.Fprintf(&b, " (%s)", res.Reason)
	}
	b.WriteString("\n")
	if res.Recorded != "" {
		fmt.Fprintf(&b, "recorded: %s (auto-updated from page; effective from detection time)\n", res.Recorded)
	}
	if len(res.Diff) > 0 && string(res.Diff) != "null" {
		var d struct {
			Changes    []string `json:"changes"`
			Suspicious []string `json:"suspicious"`
		}
		if json.Unmarshal(res.Diff, &d) == nil {
			for _, c := range d.Changes {
				fmt.Fprintf(&b, "  change: %s\n", c)
			}
			for _, s := range d.Suspicious {
				fmt.Fprintf(&b, "  suspicious: %s\n", s)
			}
		}
	}
	fmt.Fprintf(&b, "prices: %s\n", res.Status.Version)
	_, err := io.WriteString(out, b.String())
	return err
}
