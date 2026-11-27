package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/providers"
)

func TestForkUsageSteeringLimitsJSONAndText(t *testing.T) {
	daemon := newForkProvidersDaemon(t, "ok", true)
	// Providers off: the steering lines still show the DeepSeek window.
	out, _, err := runForkProvidersCLI(t, daemon, "usage", "limits")
	if err != nil || strings.Contains(out, "PROVIDER") || !strings.Contains(out, "steer: deepseek ") ||
		!strings.Contains(out, "openrouter UNKNOWN (key polling disabled") {
		t.Fatalf("limits with providers off: %v\n%s", err, out)
	}
	for _, p := range []string{"deepseek", "openrouter"} {
		if _, _, err := runForkProvidersCLI(t, daemon, "usage", "providers", "enable", p); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err = runForkProvidersCLI(t, daemon, "usage", "limits")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("limits:\n%s", out)
	for _, want := range []string{"steer: deepseek ", "runway: insufficient history", "       openrouter OK ($73.20 of $100.00 left (weekly)", "model prices: listing not fetched yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("limits missing %q:\n%s", want, out)
		}
	}
	jsonOut, _, err := runForkProvidersCLI(t, daemon, "usage", "limits", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Accounts []map[string]any   `json:"accounts"`
		Steering providers.Steering `json:"steering"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &res); err != nil || len(res.Accounts) != 1 {
		t.Fatalf("limits --json: %v\n%s", err, jsonOut)
	}
	d, o := res.Steering.DeepSeek, res.Steering.OpenRouter
	if res.Steering.Schema != 1 || d == nil || d.Window == "" || d.PriceFactor == nil || d.Balance == nil || d.Balance.Amount != "42.10" ||
		d.Runway.Reason != "insufficient history" || d.Advice.Code == "" || d.WindowEndsAtBerlin == "" {
		t.Fatalf("deepseek steering %+v", d)
	}
	if o == nil || o.Advice.Code != providers.AdviceOK || o.Key == nil || o.Key.RemainingUSD != 73.2 || o.Runway.Days == nil || o.ListingSource == "" {
		t.Fatalf("openrouter steering %+v", o)
	}
	if strings.Contains(jsonOut, "SENTINEL") {
		t.Fatal("key leaked into the steering JSON")
	}
}

func TestForkUsageSteeringProvidersSet(t *testing.T) {
	daemon := newForkProvidersDaemon(t, "ok", true)
	out, _, err := runForkProvidersCLI(t, daemon, "usage", "providers", "set", "runwayLowDays=5", "balanceLow=10", "modelsListing=off")
	if err != nil || !strings.Contains(out, "runwayLowDays=5 balanceLow=10 keyRemainingLowPct=25") || !strings.Contains(out, "model listing: off") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"bogus=1"}, {"runwayLowDays=x"}, {"modelsListing=maybe"}, {"noequals"}} {
		if _, _, err := runForkProvidersCLI(t, daemon, append([]string{"usage", "providers", "set"}, args...)...); ExitCode(err) != 2 {
			t.Errorf("%v: exit %d %v", args, ExitCode(err), err)
		}
	}
	if _, _, err := runForkProvidersCLI(t, daemon, "usage", "providers", "set", "balanceLow=-3"); err == nil || !strings.Contains(err.Error(), "balanceLow") {
		t.Errorf("daemon-side validation: %v", err)
	}
	if _, _, err := runForkProvidersCLI(t, daemon, "usage", "providers", "set", "runwayLowDays=1", "--host", "studio"); err == nil ||
		!strings.Contains(err.Error(), "cannot run against a remote host") {
		t.Errorf("remote set: %v", err)
	}
}

func forkSteeringHost(t *testing.T, name string, st providers.Steering) fleetUsageHost {
	t.Helper()
	data, err := json.Marshal(providers.StatusResponse{ProvidersSchema: 1, Steering: &st})
	if err != nil {
		t.Fatal(err)
	}
	return fleetUsageHost{remoteHealth: remoteHealth{Name: name, Status: remoteStatusOK}, Data: data}
}

func TestForkUsageSteeringFleetOnePerKey(t *testing.T) {
	early := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	late := early.Add(10 * time.Minute)
	ds := func(key string, at *time.Time, version, code string) *providers.DeepSeekSteering {
		return &providers.DeepSeekSteering{KeyID: key, FetchedAt: at, Window: "peak", Advice: providers.Advice{Code: code, Reason: code},
			PriceBook: &providers.SteerBook{Version: version}}
	}
	or := func(key string) *providers.OpenRouterSteering {
		return &providers.OpenRouterSteering{KeyID: key, Advice: providers.Advice{Code: "ok", Reason: "fine"}, ListingStatus: providers.ListingOK,
			Models: []providers.SteerModel{{Model: "deepseek/deepseek-v4.1-flash", BlendedPerM: "0.08112", Rank: 1}}}
	}
	hosts := []fleetUsageHost{
		forkSteeringHost(t, "local", providers.Steering{Schema: 1, DeepSeek: ds("aaaaaaaaaaaa", &early, "v1", "defer-nonurgent"), OpenRouter: or("")}),
		forkSteeringHost(t, "studio", providers.Steering{Schema: 1, DeepSeek: ds("aaaaaaaaaaaa", &late, "v2", "low-runway"), OpenRouter: or("bbbbbbbbbbbb")}),
		forkSteeringHost(t, "mini", providers.Steering{Schema: 1, DeepSeek: ds("", nil, "v2", "defer-nonurgent"), OpenRouter: or("bbbbbbbbbbbb")}),
		{remoteHealth: remoteHealth{Name: "gone", Status: remoteStatusOffline}},
	}
	block := forkFleetSteering(hosts)
	if len(block.DeepSeek) != 1 || block.DeepSeek[0].Advice.Code != "low-runway" || strings.Join(block.DeepSeek[0].Hosts, ",") != "local,studio" {
		t.Fatalf("deepseek %+v", block.DeepSeek)
	}
	if len(block.OpenRouter) != 1 || strings.Join(block.OpenRouter[0].Hosts, ",") != "studio,mini" {
		t.Fatalf("openrouter %+v", block.OpenRouter)
	}
	if len(block.Warnings) != 1 || !strings.Contains(block.Warnings[0], "v1 on local") {
		t.Fatalf("warnings %v", block.Warnings)
	}
	var b strings.Builder
	if err := writeForkSteering(&b, block.DeepSeek, block.OpenRouter, block.Warnings, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"steer: [local, studio] deepseek LOW-RUNWAY", "       [studio, mini] openrouter OK (fine; cheapest allowed: deepseek/deepseek-v4.1-flash $0.08/M blended)", "warning: hosts use different"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q:\n%s", want, b.String())
		}
	}
}
