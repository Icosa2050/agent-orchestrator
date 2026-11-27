package cli

// Fork-only (Icosa2050, see FORK.md "Usage seams"): the steering block of
// `ao usage limits` (design §9: price window, runway, advice codes, the
// OpenRouter model ranking) and `ao usage providers set`.

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/providers"
)

const forkProvidersSettingsPath = forkProvidersPath + "/settings"

func newForkUsageProvidersSetCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "set key=value...",
		Short: "Set steering thresholds or switch the OpenRouter model listing",
		Long: "Set the steering settings behind the `ao usage limits` advice codes:\n" +
			"  runwayLowDays=3       low-runway below this many days (DeepSeek balance, OpenRouter limit)\n" +
			"  balanceLow=5          DeepSeek low-runway below this balance (account currency)\n" +
			"  keyRemainingLowPct=25 OpenRouter low-runway below this share of the limit left\n" +
			"  modelsListing=on|off  daily keyless fetch of https://openrouter.ai/api/v1/models for model prices (default on)\n" +
			"prefer-cheaper (OpenRouter below 50 % left) is fixed.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set, err := parseForkSteeringSettings(args)
			if err == nil {
				err = set.Validate()
			}
			if err != nil {
				return usageError{err}
			}
			var raw json.RawMessage
			if err := ctx.postJSON(cmd.Context(), forkProvidersSettingsPath, set, &raw); err != nil {
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

func parseForkSteeringSettings(args []string) (providers.Settings, error) {
	var set providers.Settings
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if !ok || value == "" {
			return set, fmt.Errorf("want key=value, got %q", arg)
		}
		switch key {
		case "runwayLowDays", "keyRemainingLowPct":
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return set, fmt.Errorf("%s must be a number, got %q", key, value)
			}
			if key == "runwayLowDays" {
				set.RunwayLowDays = &f
			} else {
				set.KeyRemainingLowPct = &f
			}
		case "balanceLow":
			v := value
			set.BalanceLow = &v
		case "modelsListing":
			var on bool
			switch strings.ToLower(value) {
			case "on", "true", "1":
				on = true
			case "off", "false", "0":
			default:
				return set, fmt.Errorf("modelsListing must be on or off, got %q", value)
			}
			set.ModelsListing = &on
		default:
			return set, fmt.Errorf("unknown setting %q; use runwayLowDays, balanceLow, keyRemainingLowPct or modelsListing", key)
		}
	}
	return set, nil
}

// forkSteeringBlock is the fleet form: one steering entry per distinct key.
type forkSteeringBlock struct {
	Schema     int                             `json:"schema"`
	DeepSeek   []*providers.DeepSeekSteering   `json:"deepseek"`
	OpenRouter []*providers.OpenRouterSteering `json:"openrouter"`
	Warnings   []string                        `json:"warnings,omitempty"`
}

type forkDeepSeekHostEntry struct {
	entry *providers.DeepSeekSteering
	host  string
	th    providers.Thresholds
}

// forkFleetSteering keeps one steering entry per provider and key id (the
// newest reading wins; entries without a key collapse to one), listing the
// hosts that report it, and warns when hosts' price-book versions differ. A
// DeepSeek entry without a runway borrows another host's spend history for
// the same wallet (BorrowRunway), keeping its own balance and advice.
func forkFleetSteering(hosts []fleetUsageHost) forkSteeringBlock {
	out := forkSteeringBlock{Schema: providers.SteeringSchema, DeepSeek: []*providers.DeepSeekSteering{}, OpenRouter: []*providers.OpenRouterSteering{}}
	dsIdx, orIdx := map[string]int{}, map[string]int{}
	var dsAll [][]forkDeepSeekHostEntry // every host's entry per wallet, for runway history
	versions := map[string][]string{}
	for _, h := range hosts {
		if h.Status != remoteStatusOK || len(h.Data) == 0 {
			continue
		}
		var res providers.StatusResponse
		if json.Unmarshal(h.Data, &res) != nil || res.Steering == nil {
			continue
		}
		if d := res.Steering.DeepSeek; d != nil {
			if d.PriceBook != nil && d.PriceBook.Version != "" {
				versions[d.PriceBook.Version] = append(versions[d.PriceBook.Version], h.Name)
			}
			hostEntry := forkDeepSeekHostEntry{entry: d, host: h.Name, th: res.Steering.Thresholds.Thresholds}
			if i, ok := dsIdx[d.KeyID]; ok {
				dsAll[i] = append(dsAll[i], hostEntry)
				seen := append(append([]string{}, out.DeepSeek[i].Hosts...), h.Name)
				// Same wallet: the newest reading is authoritative.
				if newer(d.FetchedAt, out.DeepSeek[i].FetchedAt) {
					out.DeepSeek[i] = d
				}
				out.DeepSeek[i].Hosts = seen
			} else {
				d.Hosts = []string{h.Name}
				dsIdx[d.KeyID] = len(out.DeepSeek)
				out.DeepSeek = append(out.DeepSeek, d)
				dsAll = append(dsAll, []forkDeepSeekHostEntry{hostEntry})
			}
		}
		if o := res.Steering.OpenRouter; o != nil {
			if i, ok := orIdx[o.KeyID]; ok {
				seen := append(append([]string{}, out.OpenRouter[i].Hosts...), h.Name)
				if newer(o.FetchedAt, out.OpenRouter[i].FetchedAt) {
					out.OpenRouter[i] = o
				}
				out.OpenRouter[i].Hosts = seen
			} else {
				o.Hosts = []string{h.Name}
				orIdx[o.KeyID] = len(out.OpenRouter)
				out.OpenRouter = append(out.OpenRouter, o)
			}
		}
	}
	// A host with sparse balance history borrows the newest runway another
	// host computed for the same wallet, against its own (newest) balance.
	for i, entries := range dsAll {
		var donor *forkDeepSeekHostEntry
		for j := range entries {
			e := &entries[j]
			if e.entry != out.DeepSeek[i] && e.entry.Runway.Days != nil && (donor == nil || newer(e.entry.FetchedAt, donor.entry.FetchedAt)) {
				donor = e
			}
		}
		if donor != nil && out.DeepSeek[i].KeyID != "" {
			th := providers.DefaultThresholds
			for _, e := range entries {
				if e.entry == out.DeepSeek[i] {
					th = e.th
				}
			}
			out.DeepSeek[i].BorrowRunway(donor.entry, donor.host, th)
		}
	}
	// A keyed entry makes the key-less (polling off) entry of another host noise.
	out.DeepSeek = dropKeyless(out.DeepSeek, func(d *providers.DeepSeekSteering) string { return d.KeyID })
	out.OpenRouter = dropKeyless(out.OpenRouter, func(o *providers.OpenRouterSteering) string { return o.KeyID })
	if len(versions) > 1 {
		parts := make([]string, 0, len(versions))
		for v, hs := range versions {
			parts = append(parts, v+" on "+strings.Join(hs, ", "))
		}
		sort.Strings(parts)
		out.Warnings = append(out.Warnings, "hosts use different DeepSeek price books: "+strings.Join(parts, "; "))
	}
	return out
}

func dropKeyless[T any](entries []T, key func(T) string) []T {
	keyed := false
	for _, e := range entries {
		if key(e) != "" {
			keyed = true
		}
	}
	if !keyed {
		return entries
	}
	kept := entries[:0]
	for _, e := range entries {
		if key(e) != "" {
			kept = append(kept, e)
		}
	}
	return kept
}

// writeForkSteering prints the compact steering lines (design §9.6).
func writeForkSteering(out io.Writer, ds []*providers.DeepSeekSteering, or []*providers.OpenRouterSteering, warnings []string, showHosts bool) error {
	lines := make([]string, 0, len(ds)+len(or))
	for _, d := range ds {
		lines = append(lines, forkHostsPrefix(d.Hosts, showHosts)+forkDeepSeekSteerLine(d))
	}
	for _, o := range or {
		lines = append(lines, forkHostsPrefix(o.Hosts, showHosts)+forkOpenRouterSteerLine(o))
	}
	if len(lines) == 0 {
		return nil
	}
	for i, l := range lines {
		prefix := "steer: "
		if i > 0 {
			prefix = "       "
		}
		if _, err := fmt.Fprintln(out, prefix+l); err != nil {
			return err
		}
	}
	for _, w := range warnings {
		if _, err := fmt.Fprintln(out, "warning: "+w); err != nil {
			return err
		}
	}
	return nil
}

func forkHostsPrefix(hosts []string, show bool) string {
	if !show || len(hosts) == 0 {
		return ""
	}
	return "[" + strings.Join(hosts, ", ") + "] "
}

func forkDeepSeekSteerLine(d *providers.DeepSeekSteering) string {
	parts := []string{d.Advice.Reason}
	switch d.Advice.Code {
	case providers.AdviceStop, providers.AdviceLowRunway:
		if d.Window != "" {
			parts = append(parts, "window "+d.Window)
		}
	case providers.AdviceDeferNonurgent:
		if n := d.NextOffPeak; n != nil && (d.WindowEndsAt == nil || !n.From.Equal(*d.WindowEndsAt)) {
			parts = append(parts, "next off-peak "+forkUTCBerlin(n.From, "Mon 15:04"))
		} else if n != nil {
			parts = append(parts, "off-peak until "+forkUTCBerlin(n.Until, "Mon 15:04"))
		}
	}
	if d.Runway.Days != nil {
		parts = append(parts, "runway "+strconv.FormatFloat(*d.Runway.Days, 'f', -1, 64)+" d")
	} else if d.Runway.Reason != "" && d.Balance != nil {
		parts = append(parts, "runway: "+d.Runway.Reason)
	}
	return "deepseek " + strings.ToUpper(d.Advice.Code) + " (" + strings.Join(parts, "; ") + ")"
}

func forkOpenRouterSteerLine(o *providers.OpenRouterSteering) string {
	parts := []string{o.Advice.Reason}
	if o.Runway.Days != nil {
		parts = append(parts, "runway "+strconv.FormatFloat(*o.Runway.Days, 'f', -1, 64)+" d")
	}
	switch c := o.Cheapest(); {
	case c != nil:
		blended, _ := strconv.ParseFloat(c.BlendedPerM, 64)
		parts = append(parts, fmt.Sprintf("cheapest allowed: %s $%.2f/M blended", c.Model, blended))
	case o.ListingStatus != providers.ListingOK:
		parts = append(parts, "model prices: listing "+o.ListingStatus)
	case len(o.Models) == 0:
		parts = append(parts, "no PAL allowed models found")
	}
	return "openrouter " + strings.ToUpper(o.Advice.Code) + " (" + strings.Join(parts, "; ") + ")"
}

func forkSteeringOf(res *providers.StatusResponse) ([]*providers.DeepSeekSteering, []*providers.OpenRouterSteering) {
	if res == nil || res.Steering == nil {
		return nil, nil
	}
	var ds []*providers.DeepSeekSteering
	var or []*providers.OpenRouterSteering
	if res.Steering.DeepSeek != nil {
		ds = append(ds, res.Steering.DeepSeek)
	}
	if res.Steering.OpenRouter != nil {
		or = append(or, res.Steering.OpenRouter)
	}
	return ds, or
}
