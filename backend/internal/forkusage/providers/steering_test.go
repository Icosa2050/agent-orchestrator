package providers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
)

var steerNow = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC) // Wednesday, peak

func peakWindow() *pricebook.WindowReport {
	ends := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	next := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	return &pricebook.WindowReport{
		Window: pricebook.WindowPeak, PriceFactor: "1", PeakVsOffPeak: "2",
		WindowEndsAt: &pricebook.TimePair{UTC: ends, Berlin: "2026-11-25T11:00:00+01:00"},
		NextOffPeak:  &pricebook.SpanPair{From: pricebook.TimePair{UTC: ends, Berlin: "2026-11-25T11:00:00+01:00"}, Until: pricebook.TimePair{UTC: next, Berlin: "2026-11-26T02:00:00+01:00"}},
		WindowRule:   "reply time", HolidayCalendar: "2026",
		Models:    []pricebook.WindowModel{{Model: "deepseek-flash"}},
		PriceBook: pricebook.BookRef{Version: "deepseek-2026-11-24", Source: "embedded"},
	}
}

func offPeakWindow() *pricebook.WindowReport {
	w := peakWindow()
	w.Window, w.PriceFactor = pricebook.WindowOffPeak, "0.5"
	return w
}

func dsRow(total string, available bool) Row {
	t := steerNow.Add(-time.Minute)
	return Row{Provider: DeepSeek, Status: StatusOK, KeyID: "a1b2c3d4e5f6", FetchedAt: &t, IsAvailable: &available,
		Balance: &Balance{Currency: "USD", Total: total, Granted: "0.00", ToppedUp: total}}
}

// spendSamples is a 7-day history spending perDay per day, ending at the
// balance total.
func spendSamples(total float64, perDay float64, days int) []Sample {
	var out []Sample
	for i := days * 4; i >= 0; i-- {
		ts := steerNow.Add(-time.Duration(i) * 6 * time.Hour)
		amount := total + perDay*float64(i)/4
		out = append(out, Sample{TS: ts, KeyID: "a1b2c3d4e5f6", Currency: "USD", Total: jsonFloat(amount), Granted: "0.00", ToppedUp: "100.00"})
	}
	return out
}

func jsonFloat(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

func orRow(limit, remaining, weekly string) Row {
	t := steerNow.Add(-time.Minute)
	n := func(s string) *json.Number {
		if s == "" {
			return nil
		}
		v := json.Number(s)
		return &v
	}
	reset := "weekly"
	resets := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	return Row{Provider: OpenRouter, Status: StatusOK, KeyID: "9f8e7d6c5b4a", FetchedAt: &t,
		Key: &KeyInfo{LimitUSD: n(limit), RemainingUSD: n(remaining), UsageWeeklyUSD: n(weekly), LimitReset: &reset, LimitResetsAt: &resets}}
}

func TestSteeringDeepSeekAdviceTable(t *testing.T) {
	stale := peakWindow()
	stale.PriceBook.Stale = true
	missing := offPeakWindow()
	missing.HolidayCalendar = "2027 missing: weekday peak ranges priced as peak (upper bound)"
	disabled := Row{Provider: DeepSeek, Status: StatusDisabled}
	for _, tc := range []struct {
		name    string
		row     Row
		window  *pricebook.WindowReport
		samples []Sample
		want    string
	}{
		{"unavailable account beats everything", dsRow("40.00", false), peakWindow(), spendSamples(40, 30, 7), AdviceStop},
		{"zero balance", dsRow("0.00", true), offPeakWindow(), nil, AdviceStop},
		{"low runway beats the window", dsRow("40.00", true), offPeakWindow(), spendSamples(40, 20, 7), AdviceLowRunway},
		{"low balance", dsRow("4.99", true), offPeakWindow(), nil, AdviceLowRunway},
		{"low balance beats a stale book", dsRow("4.99", true), stale, nil, AdviceLowRunway},
		{"stale book", dsRow("40.00", true), stale, nil, AdviceUnknown},
		{"missing holiday year", dsRow("40.00", true), missing, nil, AdviceUnknown},
		{"no window", dsRow("40.00", true), nil, nil, AdviceUnknown},
		{"peak", dsRow("40.00", true), peakWindow(), spendSamples(40, 1, 7), AdviceDeferNonurgent},
		{"off-peak", dsRow("40.00", true), offPeakWindow(), spendSamples(40, 1, 7), AdviceOKOffPeak},
		{"no balance: window codes still apply", disabled, peakWindow(), nil, AdviceDeferNonurgent},
		{"no balance off-peak", disabled, offPeakWindow(), nil, AdviceOKOffPeak},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := buildSteering(steerInputs{now: steerNow, th: DefaultThresholds, window: tc.window, deepseek: tc.row, samples: tc.samples,
				openrouter: Row{Provider: OpenRouter, Status: StatusDisabled}, listingOn: true})
			if got := st.DeepSeek.Advice; got.Code != tc.want || got.Reason == "" {
				t.Fatalf("advice %+v, want %s (runway %+v)", got, tc.want, st.DeepSeek.Runway)
			}
		})
	}
}

func TestSteeringDeepSeekWindowFieldsAndRunway(t *testing.T) {
	st := buildSteering(steerInputs{now: steerNow, th: DefaultThresholds, window: peakWindow(), deepseek: dsRow("42.10", true),
		samples: spendSamples(42.10, 3.42, 7), openrouter: Row{Provider: OpenRouter, Status: StatusDisabled}})
	d := st.DeepSeek
	if d.Window != "peak" || *d.PriceFactor != 1 || *d.PeakVsOffPeak != 2 || d.WindowEndsAtBerlin != "2026-11-25T11:00:00+01:00" ||
		d.NextOffPeak == nil || d.NextOffPeak.UntilBerlin != "2026-11-26T02:00:00+01:00" || d.Balance.Amount != "42.10" {
		t.Fatalf("window fields %+v", d)
	}
	if d.Runway.Days == nil || *d.Runway.Days != 12.3 || d.Runway.AvgDailySpend != "3.42" || *d.Runway.CoverageDays != 7 {
		t.Fatalf("runway %+v", d.Runway)
	}
	if !strings.Contains(d.Advice.Reason, "Wed 10:00 UTC (11:00 Berlin)") || !strings.Contains(d.Advice.Reason, "half price") {
		t.Fatalf("reason %q", d.Advice.Reason)
	}
	if st.Thresholds.PreferCheaperPct != 50 || st.Thresholds.RunwayLowDays != 3 {
		t.Fatalf("thresholds %+v", st.Thresholds)
	}
}

func TestSteeringDeepSeekRunwayNullCases(t *testing.T) {
	key, cur := "a1b2c3d4e5f6", "USD"
	bal, _ := parseDecimal("40.00")
	short := spendSamples(40, 2, 1)
	if r := deepseekRunway(short, key, cur, bal, steerNow); r.Days != nil || r.Reason != "insufficient history" {
		t.Fatalf("1 day of history: %+v", r)
	}
	// Every interval holds a top-up: none counts as spend history.
	var topped []Sample
	for i, s := range spendSamples(40, 2, 7) {
		s.ToppedUp = jsonFloat(float64(100 + i))
		topped = append(topped, s)
	}
	if r := deepseekRunway(topped, key, cur, bal, steerNow); r.Days != nil || r.Reason != "insufficient history" {
		t.Fatalf("adjustment-only intervals: %+v", r)
	}
	flat := spendSamples(40, 0, 7)
	if r := deepseekRunway(flat, key, cur, bal, steerNow); r.Days != nil || r.Reason != "no spend observed" {
		t.Fatalf("flat: %+v", r)
	}
	// Another key's samples are ignored (rotation starts a new baseline).
	if r := deepseekRunway(spendSamples(40, 2, 7), "otherkey0000", cur, bal, steerNow); r.Days != nil {
		t.Fatalf("other key: %+v", r)
	}
	st := buildSteering(steerInputs{now: steerNow, th: DefaultThresholds, deepseek: Row{Provider: DeepSeek, Status: StatusDisabled}})
	if st.DeepSeek.Runway.Days != nil || st.DeepSeek.Runway.Reason != "balance polling disabled" || st.DeepSeek.Balance != nil {
		t.Fatalf("polling off: %+v", st.DeepSeek.Runway)
	}
}

func TestSteeringOpenRouterAdviceTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  Row
		want string
	}{
		{"exhausted", orRow("100", "0", "100"), AdviceStop},
		{"runway below 3 days", orRow("100", "60", "60"), AdviceLowRunway}, // Wed 09:00: 2.375 d elapsed, ~25/d
		{"below 25 percent", orRow("100", "20", "1"), AdviceLowRunway},
		{"below 50 percent", orRow("100", "40", "1"), AdvicePreferCheaper},
		{"plenty", orRow("100", "73.2", "3.1"), AdviceOK},
		{"no limit", orRow("", "", "3.1"), AdviceOK},
		{"disabled", Row{Provider: OpenRouter, Status: StatusDisabled}, AdviceUnknown},
		{"never read", Row{Provider: OpenRouter, Status: StatusUnavailable, Reason: ReasonNoKey}, AdviceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := buildSteering(steerInputs{now: steerNow, th: DefaultThresholds, openrouter: tc.row, deepseek: Row{Provider: DeepSeek, Status: StatusDisabled}})
			if got := st.OpenRouter.Advice; got.Code != tc.want || got.Reason == "" {
				t.Fatalf("advice %+v, want %s (key %+v runway %+v)", got, tc.want, st.OpenRouter.Key, st.OpenRouter.Runway)
			}
		})
	}
	st := buildSteering(steerInputs{now: steerNow, th: DefaultThresholds, openrouter: orRow("100", "73.2", "3.1")})
	o := st.OpenRouter
	if o.Key == nil || o.Key.RemainingPct != 73.2 || o.Key.ResetsAtBerlin != "2026-11-30T01:00:00+01:00" || o.Runway.Days == nil || *o.Runway.Days != 56.1 {
		t.Fatalf("key/runway %+v %+v", o.Key, o.Runway)
	}
	// The effective budget binds when its remaining share is smaller.
	row := orRow("100", "73.2", "3.1")
	l, r := json.Number("10"), json.Number("2")
	row.Key.Budget = &Budget{LimitUSD: &l, RemainingUSD: &r}
	if k := bindingLimit(row.Key); k.LimitBasis != "effective_budget" || k.RemainingPct != 20 {
		t.Fatalf("binding %+v", k)
	}
}

func writePALFixture(t *testing.T) PALConfigPaths {
	t.Helper()
	dir := t.TempDir()
	reg := filepath.Join(dir, "openrouter_models.json")
	regJSON := `{"models":[
	  {"model_name":"@preset/pal-cheap","description":"OpenRouter preset pal-cheap: deepseek/deepseek-v4.1-flash -> z-ai/glm-5.3-flash -> qwen/qwen3.8-27b (capabilities = weakest of the three)"},
	  {"model_name":"@preset/pal-balanced","description":"OpenRouter preset pal-balanced: ~deepseek/deepseek-pro-latest -> z-ai/glm-5.3 (x)"},
	  {"model_name":"@preset/pal-strong","description":"OpenRouter preset other-name: x-ai/grok-4.6"}]}`
	if err := os.WriteFile(reg, []byte(regJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(dir, "claude.json")
	doc := map[string]any{"mcpServers": map[string]any{"pal": map[string]any{"env": map[string]string{
		"OPENROUTER_ALLOWED_MODELS":     "~deepseek/deepseek-pro-latest, deepseek/deepseek-v4.1-flash,qwen/qwen3.8-27b,x-ai/grok-4.6,missing/model-x,@preset/pal-cheap,@preset/pal-balanced,@preset/pal-strong,bad entry",
		"OPENROUTER_MODELS_CONFIG_PATH": reg,
		"OPENROUTER_API_KEY":            sentinelKey,
	}}}}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(claude, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return PALConfigPaths{ClaudeJSON: claude}
}

func TestSteeringOpenRouterModelsRankingAndPresets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "openrouter-models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resp openrouterModelsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatal(err)
	}
	models, ok := parseListing(resp)
	if !ok || len(models) != 6 {
		t.Fatalf("listing %v %+v", ok, models) // the -1 router and the bad id are dropped
	}
	pal := readPALConfig(writePALFixture(t))
	if len(pal.Allowed) != 8 || pal.Reason != "" || len(pal.Presets) != 2 {
		t.Fatalf("pal config %+v", pal)
	}
	st := buildSteering(steerInputs{now: steerNow, th: DefaultThresholds, pal: pal, listingOn: true, listingOK: true,
		listing: listingFile{FetchedAt: steerNow.Add(-time.Hour), Models: models}})
	o := st.OpenRouter
	got := map[string]SteerModel{}
	for _, m := range o.Models {
		got[m.Model] = m
	}
	flash := got["deepseek/deepseek-v4.1-flash"]
	if flash.Rank != 1 || flash.PromptPerM != "0.0507" || flash.CompletionPerM != "0.2028" || flash.CacheReadPerM != "0.001014" || flash.BlendedPerM != "0.08112" {
		t.Fatalf("flash %+v", flash)
	}
	if got["qwen/qwen3.8-27b"].Rank != 2 || got["~deepseek/deepseek-pro-latest"].Rank != 3 || got["x-ai/grok-4.6"].Rank != 4 {
		t.Fatalf("ranking %+v", o.Models)
	}
	if m := got["missing/model-x"]; m.Status != ModelNotListed || m.Rank != 0 {
		t.Fatalf("missing %+v", m)
	}
	if c := o.Cheapest(); c == nil || c.Model != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("cheapest %+v", c)
	}
	if len(o.Presets) != 3 || o.Presets[0].Name != "pal-cheap" || len(o.Presets[0].Chain) != 3 ||
		o.Presets[0].Chain[0].BlendedPerM != "0.08112" || len(o.Presets[2].Chain) != 0 {
		t.Fatalf("presets %+v", o.Presets)
	}
	// Cross-route: OpenRouter's DeepSeek listings next to the direct prices.
	if a := st.DeepSeek.AlsoVia; a == nil || a.Route != "openrouter" || len(a.Models) != 3 || a.Note != CrossRouteNote {
		t.Fatalf("alsoVia %+v", a)
	}
	if out, _ := json.Marshal(st); strings.Contains(string(out), "SENTINEL") {
		t.Fatal("the PAL env key leaked into the steering block")
	}
	// Listing off and never fetched: models stay listed by name, unpriced.
	st = buildSteering(steerInputs{now: steerNow, th: DefaultThresholds, pal: pal})
	if st.OpenRouter.ListingStatus != ListingDisabled || st.OpenRouter.Models[0].Status != ModelUnpriced || st.DeepSeek.AlsoVia != nil {
		t.Fatalf("listing off %+v", st.OpenRouter)
	}
}

func TestReadPALConfigMissing(t *testing.T) {
	pal := readPALConfig(PALConfigPaths{ClaudeJSON: filepath.Join(t.TempDir(), "nope.json")})
	if len(pal.Allowed) != 0 || !strings.Contains(pal.Reason, "no PAL MCP config") {
		t.Fatalf("%+v", pal)
	}
}

func TestModelsListingKeylessDailyAndSwitch(t *testing.T) {
	f := newFakeProviders(t)
	clock := &testClock{t: steerNow}
	svc := newTestService(t, f, staticKeys{OpenRouter: sentinelKey}, clock, nil)
	svc.cfg.PAL = writePALFixture(t)
	ctx := context.Background()
	svc.refreshListingIfDue(ctx)
	svc.refreshListingIfDue(ctx) // fresh: not again
	clock.add(23 * time.Hour)
	svc.refreshListingIfDue(ctx)
	if n := f.requests(); n != 1 {
		t.Fatalf("%d listing requests within a day, want 1", n)
	}
	clock.add(time.Hour)
	svc.refreshListingIfDue(ctx)
	if n := f.requests(); n != 2 {
		t.Fatalf("%d listing requests after a day, want 2", n)
	}
	f.mu.Lock()
	for i, p := range f.paths {
		if p != "/api/v1/models" || f.auths[i] != "" {
			t.Errorf("listing request %s carried auth %v", p, f.auths[i] != "")
		}
	}
	f.mu.Unlock()
	res := svc.Status(ctx, false)
	if o := res.Steering.OpenRouter; o.ListingStatus != ListingOK || o.Cheapest() == nil || !res.ModelsListing {
		t.Fatalf("steering %+v", o)
	}
	// A failure keeps the cached listing and retries after an hour only.
	f.set("malformed")
	clock.add(ListingEvery)
	svc.refreshListingIfDue(ctx)
	clock.add(30 * time.Minute)
	svc.refreshListingIfDue(ctx)
	if n := f.requests(); n != 3 {
		t.Fatalf("%d requests after a failure, want 3", n)
	}
	if o := svc.Status(ctx, false).Steering.OpenRouter; o.ListingStatus != ListingOK || !strings.Contains(o.ListingReason, "last refresh failed") {
		t.Fatalf("after failure %+v", o)
	}
	// Redirects are refused.
	f.set("redirect")
	clock.add(2 * time.Hour)
	svc.refreshListingIfDue(ctx)
	if l, _ := readListing(svc.cfg.DataDir); l.LastReason != ReasonRedirect {
		t.Fatalf("redirect: %+v", l.LastReason)
	}
	// Switch off: no request at all.
	f.set("ok")
	off := false
	if err := svc.SetSettings(Settings{ModelsListing: &off}); err != nil {
		t.Fatal(err)
	}
	before := f.requests()
	clock.add(48 * time.Hour)
	svc.refreshListingIfDue(ctx)
	if f.requests() != before {
		t.Fatal("listing fetched with the switch off")
	}
	if res := svc.Status(ctx, false); res.ModelsListing || !strings.Contains(res.Steering.OpenRouter.ListingReason, "refresh is off") {
		t.Fatalf("switch off %+v", res.Steering.OpenRouter)
	}
}

func TestSettingsValidateAndPersist(t *testing.T) {
	dir := t.TempDir()
	neg, big, pct := -1.0, 101.0, 30.0
	bad := []Settings{{}, {RunwayLowDays: &neg}, {KeyRemainingLowPct: &big}}
	for _, b := range []string{"-1", "1e3", "", "abc"} {
		bad = append(bad, Settings{BalanceLow: &b})
	}
	for _, s := range bad {
		if err := writeSettings(dir, s); err == nil {
			t.Errorf("accepted %+v", s)
		}
	}
	days, low := 5.0, "10"
	if err := writeSettings(dir, Settings{RunwayLowDays: &days, BalanceLow: &low, KeyRemainingLowPct: &pct}); err != nil {
		t.Fatal(err)
	}
	if err := writeEnabled(dir, DeepSeek, true); err != nil {
		t.Fatal(err)
	}
	enabled, th, listing := readSettings(dir)
	if !enabled[DeepSeek] || th.RunwayLowDays != 5 || th.BalanceLow != "10" || th.KeyRemainingLowPct != 30 || !listing {
		t.Fatalf("settings %v %+v %v", enabled, th, listing)
	}
}

// docs/provider-usage.md carries the orchestrator rule text; it must name
// every advice code.
func TestSteeringRuleTextNamesEveryAdviceCode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "provider-usage.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range AdviceCodes {
		if !strings.Contains(string(data), "`"+code+"`") {
			t.Errorf("docs/provider-usage.md does not name advice code %q", code)
		}
	}
}
