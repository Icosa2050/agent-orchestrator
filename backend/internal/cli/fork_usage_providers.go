package cli

// Fork-only (Icosa2050, see FORK.md "Usage seams"): `ao usage providers`
// and the provider rows `ao usage limits` appends, thin clients of the
// daemon's /api/v1/usage/fork/providers route.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // Europe/Berlin display in packaged builds.

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/providers"
)

const forkProvidersPath = "usage/fork/providers"

var forkBerlin = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return time.UTC
	}
	return loc
}()

func newForkUsageProvidersCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "Opt in to API balance polling: DeepSeek balance and OpenRouter key limit (default off)",
		Long: "Provider polling is off by default and opt-in per provider. When enabled, the daemon reads your own API key for\n" +
			"each request (DeepSeek: the DeepSeek Harness credential store, then DEEPSEEK_API_KEY; OpenRouter: the keychain item\n" +
			"OPENROUTER_API_KEY PAL uses, then OPENROUTER_API_KEY) and sends it only to that provider's own API:\n" +
			"  deepseek    GET https://api.deepseek.com/user/balance   every 15 min, plus on demand\n" +
			"  openrouter  GET https://openrouter.ai/api/v1/key        on demand, cached 5 min\n" +
			"Keys are never stored or logged. `ao usage limits` shows the readings.",
	}
	cmd.AddCommand(markRemoteReadOnly(newForkUsageProvidersStatusCommand(ctx)))
	// enable/disable write a local decision about your keys: never remotely.
	for _, on := range []bool{true, false} {
		cmd.AddCommand(newForkUsageProvidersSwitchCommand(ctx, on))
	}
	cmd.AddCommand(newForkUsageProvidersSetCommand(ctx))
	return cmd
}

func newForkUsageProvidersStatusCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show which providers are polled and their last reading",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var raw json.RawMessage
			if err := ctx.getJSON(cmd.Context(), forkProvidersPath, &raw); err != nil {
				return forkProvidersRouteError(err)
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), raw)
			}
			var res providers.StatusResponse
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode providers: %w", err)
			}
			return writeForkProvidersStatus(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	return cmd
}

func newForkUsageProvidersSwitchCommand(ctx *commandContext, on bool) *cobra.Command {
	verb, short := "disable", "Stop polling a provider (its balance history stays)"
	if on {
		verb, short = "enable", "Start polling a provider with your own API key (deepseek or openrouter)"
	}
	var asJSON bool
	cmd := &cobra.Command{
		Use:       verb + " deepseek|openrouter",
		Short:     short,
		Args:      cobra.ExactArgs(1),
		ValidArgs: providers.Known,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.ToLower(strings.TrimSpace(args[0]))
			if name != providers.DeepSeek && name != providers.OpenRouter {
				return usageError{fmt.Errorf("unknown provider %q; use deepseek or openrouter", args[0])}
			}
			var raw json.RawMessage
			if err := ctx.postJSON(cmd.Context(), forkProvidersPath, map[string]any{"provider": name, "enabled": on}, &raw); err != nil {
				return forkProvidersRouteError(err)
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), raw)
			}
			var res providers.StatusResponse
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode providers: %w", err)
			}
			out := cmd.OutOrStdout()
			if on {
				_, _ = fmt.Fprintf(out, "%s polling enabled: the daemon reads your key per request and sends it only to %s.\n", name, forkProviderHost(name))
			} else {
				_, _ = fmt.Fprintf(out, "%s polling disabled.\n", name)
			}
			return writeForkProvidersStatus(out, res)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	return cmd
}

func forkProviderHost(name string) string {
	if name == providers.DeepSeek {
		return "api.deepseek.com"
	}
	return "openrouter.ai"
}

func forkProvidersRouteError(err error) error {
	var apiErr apiResponseError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return errors.New("this daemon has no provider polling; update it")
	}
	return err
}

func writeForkProvidersStatus(out io.Writer, res providers.StatusResponse) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROVIDER\tSTATUS\tKEY\tREAD\tNOTE")
	for _, r := range res.Providers {
		read := "-"
		if r.FetchedAt != nil {
			read = r.FetchedAt.Local().Format("Mon 15:04")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Provider, r.Status, dash(r.KeyID), read, dash(r.Reason))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	th := res.Thresholds
	_, err := fmt.Fprintf(out, "thresholds: runwayLowDays=%s balanceLow=%s keyRemainingLowPct=%s (prefer-cheaper below %d%%)\n",
		strconv.FormatFloat(th.RunwayLowDays, 'f', -1, 64), th.BalanceLow, strconv.FormatFloat(th.KeyRemainingLowPct, 'f', -1, 64), providers.PreferCheaperPct)
	if err != nil || res.Steering == nil {
		return err
	}
	listing := "on"
	if !res.ModelsListing {
		listing = "off"
	}
	_, err = fmt.Fprintf(out, "openrouter model listing: %s (keyless, at most daily; `ao usage providers set modelsListing=on|off`)\n", listing)
	return err
}

// forkProvidersFetch reads one host's provider rows. A daemon without the
// route (404) is "unsupported"; any other failure is reported, never fatal.
func forkProvidersFetch(c context.Context, ctx *commandContext) (*providers.StatusResponse, string) {
	var res providers.StatusResponse
	if err := ctx.getJSON(c, forkProvidersPath, &res); err != nil {
		var apiErr apiResponseError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, providers.StatusUnsupported
		}
		return nil, "unavailable: " + err.Error()
	}
	return &res, ""
}

// writeForkUsageLimits finishes `ao usage limits` (current mode, one host):
// the upstream table or JSON plus the provider rows. Providers never fail
// the command.
func writeForkUsageLimits(c context.Context, ctx *commandContext, out io.Writer, accounts json.RawMessage, asJSON bool) error {
	res, problem := forkProvidersFetch(c, ctx)
	if asJSON {
		merged := map[string]json.RawMessage{}
		if err := json.Unmarshal(accounts, &merged); err != nil {
			return fmt.Errorf("decode accounts: %w", err)
		}
		merged["providersSchema"] = json.RawMessage(strconv.Itoa(providers.SchemaVersion))
		rows := []providers.Row{}
		if res != nil {
			rows = res.Providers
		}
		merged["providers"], _ = json.Marshal(rows)
		if problem != "" {
			merged["providersStatus"], _ = json.Marshal(problem)
		}
		if res != nil && res.Steering != nil {
			merged["steering"], _ = json.Marshal(res.Steering)
		}
		return writeJSON(out, merged)
	}
	if err := writeUsageLimitsData(out, accounts, usageLimitsCurrent); err != nil {
		return err
	}
	if res == nil {
		if problem != providers.StatusUnsupported {
			_, err := fmt.Fprintf(out, "providers: %s\n", problem)
			return err
		}
		return nil
	}
	if err := writeForkProviderRows(out, res.Providers, time.Now()); err != nil {
		return err
	}
	ds, or := forkSteeringOf(res)
	return writeForkSteering(out, ds, or, nil, false)
}

// writeForkFleetUsageLimits finishes `ao usage limits --fleet` (current
// mode): each host's accounts as upstream, then the provider rows of every
// host with a key shown once ("same key on: …").
func writeForkFleetUsageLimits(c context.Context, ctx *commandContext, out io.Writer, hosts []fleetUsageHost, asJSON bool) error {
	provHosts, err := ctx.fleetGetJSON(c, forkProvidersPath)
	rows := []providers.Row{}
	missing := []forkProviderHostProblem{}
	var steering forkSteeringBlock
	if err == nil {
		rows = forkDedupeProviderRows(provHosts)
		missing = forkProviderProblems(hosts, provHosts)
		steering = forkFleetSteering(provHosts)
	}
	if asJSON {
		return writeJSON(out, map[string]any{"hosts": hosts, "providersSchema": providers.SchemaVersion, "providers": rows, "providersMissing": missing, "steering": steering})
	}
	if err := writeFleetUsageLimits(out, hosts, usageLimitsCurrent); err != nil {
		return err
	}
	if len(forkShownRows(rows)) > 0 {
		if _, err := fmt.Fprintln(out, "\n== providers (each key once) =="); err != nil {
			return err
		}
		if err := writeForkProviderRows(out, rows, time.Now()); err != nil {
			return err
		}
	}
	for _, m := range missing {
		if _, err := fmt.Fprintf(out, "providers not read from %s: %s\n", m.Name, m.Status); err != nil {
			return err
		}
	}
	return writeForkSteering(out, steering.DeepSeek, steering.OpenRouter, steering.Warnings, true)
}

// forkProviderHostProblem names a host whose accounts answered but whose
// provider rows did not (an older daemon, or an error).
type forkProviderHostProblem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func forkProviderProblems(accounts, provs []fleetUsageHost) []forkProviderHostProblem {
	answered := map[string]bool{}
	for _, h := range accounts {
		answered[h.Name] = h.Status == remoteStatusOK
	}
	out := []forkProviderHostProblem{}
	for _, h := range provs {
		if !answered[h.Name] || (h.Status == remoteStatusOK && len(h.Data) > 0) {
			continue
		}
		p := forkProviderHostProblem{Name: h.Name, Status: h.Status, Error: h.Error}
		if strings.Contains(h.Error, "404") || strings.Contains(strings.ToLower(h.Error), "not found") {
			p.Status, p.Error = providers.StatusUnsupported, ""
		}
		out = append(out, p)
	}
	return out
}

// forkDedupeProviderRows merges the hosts' provider rows: one row per
// provider and key id (the newest reading wins), listing the hosts that
// report it. Rows without a key id stay per host. Disabled rows are kept
// once per provider only when no host polls it.
func forkDedupeProviderRows(hosts []fleetUsageHost) []providers.Row {
	byKey := map[string]int{}
	var out []providers.Row
	for _, h := range hosts {
		if h.Status != remoteStatusOK || len(h.Data) == 0 {
			continue
		}
		var res providers.StatusResponse
		if json.Unmarshal(h.Data, &res) != nil {
			continue
		}
		for _, r := range res.Providers {
			id := r.Provider + "\x00key\x00" + r.KeyID
			if r.KeyID == "" {
				id = r.Provider + "\x00host\x00" + h.Name
				if r.Status == providers.StatusDisabled {
					id = r.Provider + "\x00disabled"
				}
			}
			if i, ok := byKey[id]; ok {
				seenOn := make([]string, 0, len(out[i].Hosts)+1)
				seenOn = append(seenOn, out[i].Hosts...)
				seenOn = append(seenOn, h.Name)
				if newer(r.FetchedAt, out[i].FetchedAt) {
					out[i] = r
				}
				out[i].Hosts = seenOn
				continue
			}
			r.Hosts = []string{h.Name}
			byKey[id] = len(out)
			out = append(out, r)
		}
	}
	// A provider some host polls makes its disabled rows noise.
	polled := map[string]bool{}
	for _, r := range out {
		if r.Status != providers.StatusDisabled {
			polled[r.Provider] = true
		}
	}
	kept := out[:0]
	for _, r := range out {
		if r.Status == providers.StatusDisabled && polled[r.Provider] {
			continue
		}
		kept = append(kept, r)
	}
	sort.SliceStable(kept, func(i, j int) bool { return forkProviderOrder(kept[i].Provider) < forkProviderOrder(kept[j].Provider) })
	return kept
}

func forkProviderOrder(name string) int {
	for i, p := range providers.Known {
		if p == name {
			return i
		}
	}
	return len(providers.Known)
}

func newer(a, b *time.Time) bool {
	return a != nil && (b == nil || a.After(*b))
}

func forkShownRows(rows []providers.Row) []providers.Row {
	var shown []providers.Row
	for _, r := range rows {
		if r.Status != providers.StatusDisabled {
			shown = append(shown, r)
		}
	}
	return shown
}

// writeForkProviderRows prints the provider table and the DeepSeek hint,
// price-book and outside-AO lines. With no provider enabled it prints
// nothing, so `ao usage limits` stays as upstream prints it.
func writeForkProviderRows(out io.Writer, rows []providers.Row, now time.Time) error {
	shown := forkShownRows(rows)
	if len(shown) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROVIDER\tKEY\tBALANCE / REMAINING\tUSED (UTC)\tRESET / WINDOW\tREAD")
	var deepseek *providers.Row
	for i := range shown {
		r := shown[i]
		if r.Provider == providers.DeepSeek && deepseek == nil {
			deepseek = &shown[i]
		}
		balance, used, reset := forkProviderCells(r)
		read := "-"
		if r.FetchedAt != nil {
			read = r.FetchedAt.Local().Format("15:04")
		}
		key := dash(r.KeyID)
		if len(r.Hosts) > 1 {
			key += " (same key on: " + strings.Join(r.Hosts, ", ") + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Provider, key, balance, used, reset, read)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if deepseek == nil {
		return nil
	}
	var lines []string
	if hint := forkWindowHint(deepseek.Window); hint != "" {
		lines = append(lines, "hint: "+hint)
	}
	if b := deepseek.PriceBook; b != nil && b.Version != "" {
		line := "prices: " + b.Version
		if b.LastCheckAt != nil {
			line += fmt.Sprintf(" (checked %dd ago)", int(now.Sub(*b.LastCheckAt).Hours()/24))
		}
		if deepseek.Window != nil && deepseek.Window.HolidayCalendar != "" {
			line += " · holidays: " + deepseek.Window.HolidayCalendar
		}
		if b.Stale {
			line += " · STALE"
		}
		lines = append(lines, line)
	}
	if o := deepseek.OutsideAO; o != nil {
		lines = append(lines, fmt.Sprintf("deepseek outside AO (%s): %s", o.Window, o.Reason))
	}
	for _, l := range lines {
		if _, err := fmt.Fprintln(out, l); err != nil {
			return err
		}
	}
	return nil
}

func forkProviderCells(r providers.Row) (balance, used, reset string) {
	balance, used, reset = "-", "-", "-"
	switch r.Provider {
	case providers.DeepSeek:
		if r.Balance != nil {
			parts := []string{fmt.Sprintf("%s %s (granted %s, topped up %s)", r.Balance.Currency, r.Balance.Total, r.Balance.Granted, r.Balance.ToppedUp)}
			for _, b := range r.Balances {
				if b.Currency != r.Balance.Currency {
					parts = append(parts, b.Currency+" "+b.Total)
				}
			}
			balance = strings.Join(parts, "; ")
			if r.IsAvailable != nil && !*r.IsAvailable {
				balance += "; NOT AVAILABLE"
			}
		}
		if ch := r.Change; ch != nil {
			if ch.Amount == nil {
				used = ch.Window + " change: " + ch.Reason
			} else {
				note := []string{"observed"}
				if cov := ch.Coverage; cov != nil {
					if cov.Partial {
						note = append(note, "since "+cov.From.UTC().Format("Mon 15:04")+" UTC only")
					}
					if cov.Gaps > 0 {
						note = append(note, fmt.Sprintf("%d gaps", cov.Gaps))
					}
				}
				if len(ch.Adjustments) > 0 {
					note = append(note, "includes "+ch.Adjustments[0].Kind)
				}
				used = fmt.Sprintf("%s change %s %s (%s)", ch.Window, forkSigned(*ch.Amount), ch.Currency, strings.Join(note, ", "))
			}
		}
		reset = forkWindowCell(r.Window)
	case providers.OpenRouter:
		if k := r.Key; k != nil {
			// Each amount is shown with its own reset period: the effective
			// budget first when OpenRouter reports one, then the key limit.
			var parts []string
			if b := k.Budget; b != nil && b.LimitUSD != nil && b.RemainingUSD != nil {
				part := fmt.Sprintf("%s left of %s", forkNumberUSD(b.RemainingUSD), forkNumberUSD(b.LimitUSD))
				if b.ResetInterval != nil {
					part += " " + *b.ResetInterval
				}
				parts = append(parts, part+" budget")
			}
			if k.LimitUSD != nil && k.RemainingUSD != nil {
				part := fmt.Sprintf("%s left of %s", forkNumberUSD(k.RemainingUSD), forkNumberUSD(k.LimitUSD))
				if k.LimitReset != nil {
					part += " " + *k.LimitReset
				}
				if len(parts) > 0 {
					part = "key " + part
				}
				parts = append(parts, part)
			}
			switch {
			case len(parts) > 0:
				balance = strings.Join(parts, "; ")
			case k.LimitUSD == nil:
				balance = "no key limit"
			}
			var u []string
			for _, p := range []struct {
				label string
				n     *json.Number
			}{{"today", k.UsageDailyUSD}, {"week", k.UsageWeeklyUSD}, {"month", k.UsageMonthlyUSD}} {
				if p.n != nil {
					u = append(u, p.label+" "+forkNumberUSD(p.n))
				}
			}
			if len(u) > 0 {
				used = strings.Join(u, ", ")
			}
			if k.ResetsAt != nil {
				reset = "resets " + forkUTCBerlin(*k.ResetsAt, "Mon 02 Jan 15:04")
				if k.ResetInterval != "" {
					reset = k.ResetInterval + ", " + reset
				}
			}
		}
	}
	if r.Status != providers.StatusOK {
		note := r.Status
		if r.Reason != "" {
			note += ": " + r.Reason
		}
		if r.Stale {
			note += " (last good reading shown)"
		}
		if balance == "-" {
			balance = note
		} else {
			balance += " [" + note + "]"
		}
	}
	return balance, used, reset
}

func forkSigned(amount string) string {
	if strings.HasPrefix(amount, "-") {
		return amount
	}
	return "+" + amount
}

func forkNumberUSD(n *json.Number) string {
	f, err := strconv.ParseFloat(string(*n), 64)
	if err != nil {
		return "$" + string(*n)
	}
	return fmt.Sprintf("$%.2f", f)
}

func forkUTCBerlin(t time.Time, layout string) string {
	return fmt.Sprintf("%s UTC (%s Berlin)", t.UTC().Format(layout), t.In(forkBerlin).Format("15:04"))
}

func forkWindowCell(w *providers.Window) string {
	if w == nil || w.Current == "" {
		return "-"
	}
	cell := strings.ToUpper(w.Current)
	if w.Until != nil {
		cell += " until " + forkUTCBerlin(*w.Until, "Mon 15:04")
	}
	return cell
}

// forkWindowHint is the one-line DeepSeek price signal.
func forkWindowHint(w *providers.Window) string {
	if w == nil || w.Current == "" {
		return ""
	}
	cheap := "half price"
	if w.PeakVsOffPeak != "" && w.PeakVsOffPeak != "2" {
		cheap = "peak is " + w.PeakVsOffPeak + "x off-peak"
	}
	switch w.Current {
	case "peak":
		if w.NextOffPeak == nil {
			return "DeepSeek is PEAK now."
		}
		return fmt.Sprintf("DeepSeek is PEAK now; off-peak (%s) from %s until %s.", cheap,
			forkUTCBerlin(w.NextOffPeak.From, "Mon 15:04"), forkUTCBerlin(w.NextOffPeak.Until, "Mon 15:04"))
	case "off-peak":
		hint := "DeepSeek is OFF-PEAK now (" + cheap + ")"
		if w.Until != nil {
			hint += " until " + forkUTCBerlin(*w.Until, "Mon 15:04")
		}
		if w.Holiday != "" {
			hint += "; holiday: " + w.Holiday
		}
		return hint + "."
	}
	return "DeepSeek window: " + w.Current + "."
}
