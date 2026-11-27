package reportext

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

var update = flag.Bool("update", false, "rewrite golden files")

func mustBook(t *testing.T) *pricebook.Book {
	t.Helper()
	b, err := pricebook.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func dayWindow(t *testing.T, from string, days int) Window {
	t.Helper()
	since := ts(t, from)
	w := Window{Days: days, TimeZone: "UTC", Resolution: "day", Since: since, Until: since.AddDate(0, 0, days)}
	for d := since; d.Before(w.Until); d = d.AddDate(0, 0, 1) {
		w.Periods = append(w.Periods, d.Format(dayLayout))
	}
	return w
}

func ev(t *testing.T, at, route, model string, hit, miss, out int64) store.ForkDeepSeekUsageEvent {
	return store.ForkDeepSeekUsageEvent{
		CreatedAt: ts(t, at), ProviderID: domain.UsageProviderOpenAI, BillingProviderID: route, ModelID: model,
		CachedTokens: hit, UncachedTokens: miss, OutputTokens: out, Harness: domain.HarnessDeepSeek, SessionID: "s1", ProjectID: "p1",
	}
}

func withStart(t *testing.T, e store.ForkDeepSeekUsageEvent, start string) store.ForkDeepSeekUsageEvent {
	s := ts(t, start)
	e.RequestStartedAt = &s
	return e
}

func build(t *testing.T, w Window, events ...store.ForkDeepSeekUsageEvent) Report {
	t.Helper()
	rep, err := Build(Input{Window: w, Book: mustBook(t), DeepSeek: events})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// readSample loads the anonymized real 30-day DeepSeek sample.
func readSample(t *testing.T) []store.ForkDeepSeekUsageEvent {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "deepseek-30d-sample.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []store.ForkDeepSeekUsageEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		c := strings.Split(line, ",")
		if len(c) != 7 {
			t.Fatalf("bad sample line %q", line)
		}
		n := func(s string) int64 {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		e := ev(t, c[0], c[1], c[2], n(c[3]), n(c[4]), n(c[5]))
		e.SessionID = domain.SessionID(c[6])
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The real 30-day sample from the design note: $21.67 with peak/off-peak
// windows vs $36.03 at peak-only rates, every event priced (both routes
// draw from the same wallet at API prices), 82 % of tokens off-peak.
func TestGoldenThirtyDaySample(t *testing.T) {
	events := readSample(t)
	if len(events) != 2140 {
		t.Fatalf("sample has %d events, want 2140", len(events))
	}
	rep := build(t, dayWindow(t, "2026-10-26T00:00:00Z", 30), events...)
	tot := rep.Totals
	if tot.Events != 2140 || tot.PricedEvents != 2140 || tot.BillingStatuses[pricebook.StatusPriced] != 2140 {
		t.Fatalf("totals events = %d priced %d statuses %v", tot.Events, tot.PricedEvents, tot.BillingStatuses)
	}
	if got := *tot.PricebookCostNanos; got != 21_674_132_620 {
		t.Errorf("windowed cost = %d nano-USD, want 21_674_132_620 ($21.67)", got)
	}
	if got := *tot.PeakListCostNanos; got != 36_033_395_244 {
		t.Errorf("peak-only cost = %d nano-USD, want 36_033_395_244 ($36.03)", got)
	}
	if tot.Peak.CostNanos+tot.OffPeak.CostNanos != *tot.PricebookCostNanos {
		t.Errorf("peak %d + off-peak %d != total %d", tot.Peak.CostNanos, tot.OffPeak.CostNanos, *tot.PricebookCostNanos)
	}
	if share := float64(tot.OffPeak.Total()) / float64(tot.Total()); share < 0.82 || share > 0.83 {
		t.Errorf("off-peak token share = %.4f, want about 0.82", share)
	}
	// Legacy rows have no request start: all are reply-time, some unverified.
	if tot.WindowBasis.RequestStart != 0 || tot.WindowBasis.ReplyTime+tot.WindowBasis.ReplyTimeUnverified != 2140 {
		t.Errorf("window basis = %+v", tot.WindowBasis)
	}
	var sum, account int64
	for _, r := range rep.PricedRows {
		sum += *r.PricebookCostNanos
		if r.Route == "deepseek-account" {
			account += *r.PricebookCostNanos
		}
		if r.Model == "deepseek-v4-flash" && r.BilledAs != "deepseek-flash" {
			t.Errorf("legacy deepseek-v4-flash billed as %q", r.BilledAs)
		}
	}
	if sum != *tot.PricebookCostNanos {
		t.Errorf("rows sum %d != total %d", sum, *tot.PricebookCostNanos)
	}
	if account != 21_498_793_528 { // $21.50
		t.Errorf("deepseek-account share = %d nano-USD, want 21_498_793_528 ($21.50)", account)
	}
	var hours int64
	for _, h := range rep.Hours {
		hours += deref(h.PricebookCostNanos)
	}
	if hours != sum {
		t.Errorf("hour histogram sums to %d, want %d", hours, sum)
	}
	golden(t, "deepseek-30d-golden.json", rep)
}

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden (run go test -update and review the diff)", name)
	}
}

func TestWindowBasis(t *testing.T) {
	w := dayWindow(t, "2026-11-23T00:00:00Z", 2) // Mon, Tue
	cases := []struct {
		name      string
		e         store.ForkDeepSeekUsageEvent
		basis     string
		window    string
		boundary  bool
		hourOfDay int
	}{
		{"request in peak, reply off-peak", withStart(t, ev(t, "2026-11-23T10:01:00Z", "deepseek-account", "deepseek-v4-pro", 0, 100, 10), "2026-11-23T09:58:00Z"),
			BasisRequestStart, pricebook.WindowPeak, false, 9},
		{"request off-peak, reply in peak", withStart(t, ev(t, "2026-11-23T01:00:30Z", "deepseek", "deepseek-flash", 0, 100, 10), "2026-11-23T00:59:00Z"),
			BasisRequestStart, pricebook.WindowOffPeak, false, 0},
		{"request and reply in one window", withStart(t, ev(t, "2026-11-23T10:02:00Z", "deepseek", "deepseek-flash", 0, 100, 10), "2026-11-23T10:00:00Z"),
			BasisReplyTime, pricebook.WindowOffPeak, false, 10},
		{"legacy 3 min after an edge", ev(t, "2026-11-23T10:03:00Z", "deepseek-account", "deepseek-v4-pro", 5, 100, 10),
			BasisReplyTimeUnverified, pricebook.WindowOffPeak, true, 10},
		{"legacy far from an edge", ev(t, "2026-11-23T12:00:00Z", "deepseek-account", "deepseek-v4-pro", 0, 100, 10),
			BasisReplyTime, pricebook.WindowOffPeak, false, 12},
		{"start after reply is ignored", withStart(t, ev(t, "2026-11-23T12:00:00Z", "deepseek", "deepseek-flash", 0, 100, 10), "2026-11-23T12:30:00Z"),
			BasisReplyTime, pricebook.WindowOffPeak, false, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := classify(mustBook(t), tc.e)
			if err != nil {
				t.Fatal(err)
			}
			if c.basis != tc.basis || c.priced.Window != tc.window || c.boundary != tc.boundary || c.windowHour() != tc.hourOfDay {
				t.Fatalf("basis %s window %s boundary %v hour %d; want %s %s %v %d",
					c.basis, c.priced.Window, c.boundary, c.windowHour(), tc.basis, tc.window, tc.boundary, tc.hourOfDay)
			}
			rep := build(t, w, tc.e)
			if want := map[bool]int64{true: 115, false: 0}[tc.boundary]; rep.Totals.BoundaryTokens != want {
				t.Fatalf("boundary tokens = %d, want %d", rep.Totals.BoundaryTokens, want)
			}
			if rep.Hours[tc.hourOfDay].Events != 1 {
				t.Fatalf("hour %d events = %d", tc.hourOfDay, rep.Hours[tc.hourOfDay].Events)
			}
		})
	}
}

func TestBillingStatusesAndVersionSplit(t *testing.T) {
	w := dayWindow(t, "2026-08-15T00:00:00Z", 30)
	rep := build(t, w,
		ev(t, "2026-08-15T12:00:00Z", "deepseek-account", "deepseek-v4-pro", 1, 1, 1), // before the first evidenced version
		ev(t, "2026-09-10T03:59:00Z", "deepseek", "deepseek-v4-flash", 0, 1000, 0),    // 2026-08-16 version, own row
		ev(t, "2026-09-10T04:01:00Z", "deepseek", "deepseek-v4-flash", 0, 1000, 0),    // 2026-09-10 version, alias
		ev(t, "2026-09-11T12:00:00Z", "deepseek", "deepseek-v9-ultra", 0, 1, 0),       // unknown model
		ev(t, "2026-09-11T12:00:00Z", "deepseek-proxy", "deepseek-flash", 0, 1, 0),    // unknown route
		ev(t, "2026-09-11T12:00:00Z", "deepseek", "", 0, 1, 0),                        // old row without model
	)
	byStatus := rep.Totals.BillingStatuses
	if byStatus[pricebook.StatusPriced] != 2 || byStatus[pricebook.StatusNoPriceEvidence] != 1 ||
		byStatus[pricebook.StatusUnknownModel] != 2 || byStatus[pricebook.StatusUnknownRoute] != 1 {
		t.Fatalf("statuses = %v", byStatus)
	}
	var split []PricedRow
	for _, r := range rep.PricedRows {
		switch {
		case r.Period == "2026-09-10":
			split = append(split, r)
		case r.BillingStatus != pricebook.StatusPriced:
			if r.PricebookCostNanos != nil || r.PricedEvents != 0 || r.Total() == 0 {
				t.Errorf("unpriced row has a cost or lost its tokens: %+v", r)
			}
		}
		if r.Model == "" && (!r.ModelMissing || r.BillingStatus != pricebook.StatusUnknownModel) {
			t.Errorf("model-less row = %+v, want modelMissing and unknown-model", r)
		}
		if r.Route == "deepseek-proxy" && r.RouteBilling != "" {
			t.Errorf("unknown route billed as %q", r.RouteBilling)
		}
	}
	if len(split) != 2 || split[0].PriceVersion == split[1].PriceVersion {
		t.Fatalf("a row spanning two price versions was not split: %+v", split)
	}
	for _, r := range split {
		if r.Model != "deepseek-v4-flash" {
			t.Errorf("raw model %q not kept", r.Model)
		}
	}
	if split[0].PriceVersion != "deepseek-2026-08-16" || split[0].BilledAs != "deepseek-v4-flash" ||
		split[1].PriceVersion != "deepseek-2026-09-10" || split[1].BilledAs != "deepseek-flash" {
		t.Errorf("versions/billedAs = %s/%s, %s/%s", split[0].PriceVersion, split[0].BilledAs, split[1].PriceVersion, split[1].BilledAs)
	}
	if len(rep.Warnings) != 3 || len(rep.PriceVersions) != 2 {
		t.Errorf("warnings = %q, versions = %q", rep.Warnings, rep.PriceVersions)
	}
}

func TestHoursHistogram(t *testing.T) {
	w := dayWindow(t, "2026-11-21T00:00:00Z", 3) // Sat, Sun, Mon
	rep := build(t, w,
		ev(t, "2026-11-21T02:30:00Z", "deepseek", "deepseek-flash", 0, 1_000_000, 0), // Saturday: peak hour, off-peak price
		ev(t, "2026-11-23T02:30:00Z", "deepseek", "deepseek-flash", 0, 1_000_000, 0), // Monday: peak
	)
	if len(rep.Hours) != 24 {
		t.Fatalf("hours = %d, want 24", len(rep.Hours))
	}
	peakHours := []int{}
	for _, h := range rep.Hours {
		if h.PeakHour {
			peakHours = append(peakHours, h.Hour)
		}
		if h.UTC != time.Date(2026, 1, 1, h.Hour, 0, 0, 0, time.UTC).Format("15:04") {
			t.Errorf("hour %d label %q", h.Hour, h.UTC)
		}
	}
	if got, want := peakHours, []int{1, 2, 3, 6, 7, 8, 9}; !equalInts(got, want) {
		t.Errorf("peak hours = %v, want %v", got, want)
	}
	h := rep.Hours[2]
	if h.Events != 2 || h.Peak.Events != 1 || h.OffPeak.Events != 1 || h.Peak.CostNanos != 300_000_000 || h.OffPeak.CostNanos != 150_000_000 {
		t.Errorf("hour 02 = %+v", h)
	}
	if rep.PeakRule == nil || rep.PeakRule.ExcludeHolidays != "cn" || len(rep.PeakRule.Weekdays) != 5 {
		t.Errorf("peak rule = %+v", rep.PeakRule)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSessionRowsAndModels(t *testing.T) {
	w := dayWindow(t, "2026-11-23T00:00:00Z", 1)
	cost := int64(777)
	a := ev(t, "2026-11-23T12:00:00Z", "deepseek-account", "deepseek-v4-pro", 0, 1_000_000, 0)
	b := ev(t, "2026-11-23T12:05:00Z", "deepseek", "deepseek-v4-flash", 0, 10, 0)
	toks := func(hit, miss, out int64) domain.UsageTokenMetrics {
		return domain.UsageTokenMetrics{CachedInputTokens: &hit, UncachedInputTokens: &miss, OutputTokens: &out}
	}
	all := []domain.UsageReportEvent{
		{CreatedAt: a.CreatedAt, ModelID: a.ModelID, Tokens: toks(0, 1_000_000, 0), Harness: domain.HarnessDeepSeek, SessionID: "s1", ProjectID: "p1"},
		{CreatedAt: b.CreatedAt, ModelID: b.ModelID, Tokens: toks(0, 10, 0), Harness: domain.HarnessDeepSeek, SessionID: "s1", ProjectID: "p1"},
		{CreatedAt: b.CreatedAt, ModelID: "claude-x", Tokens: toks(1, 2, 3), Harness: domain.HarnessClaudeCode, SessionID: "s2", ProjectID: "p1",
			Costs: domain.UsageEventCosts{EstimatedCostNanos: &cost}},
		{CreatedAt: b.CreatedAt, ModelID: "gpt-5", Tokens: toks(1, 1, 1), Harness: domain.HarnessCodex, SessionID: "s3", ProjectID: "p2"},
	}
	rep, err := Build(Input{Window: w, Book: mustBook(t), DeepSeek: []store.ForkDeepSeekUsageEvent{a, b}, All: all})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SessionRows) != 2 || rep.SessionRows[0].Model != "deepseek-v4-pro" || *rep.SessionRows[0].PricebookCostNanos != 660_000_000 {
		t.Fatalf("session rows = %+v", rep.SessionRows)
	}
	s1 := rep.SessionModels["s1"]
	if len(s1) != 2 || s1[0].Model != "deepseek-v4-pro" || s1[0].CostBasis != CostBasisPricebook || s1[1].BilledAs != "deepseek-flash" {
		t.Fatalf("s1 models = %+v", s1)
	}
	s2 := rep.SessionModels["s2"]
	if len(s2) != 1 || s2[0].CostBasis != CostBasisCatalog || *s2[0].CostNanos != 777 || s2[0].Tokens != 6 {
		t.Fatalf("s2 models = %+v", s2)
	}
	if s3 := rep.SessionModels["s3"]; len(s3) != 1 || s3[0].CostBasis != CostBasisNone || s3[0].CostNanos != nil {
		t.Fatalf("s3 models = %+v", s3)
	}

	scoped, err := Build(Input{Window: w, Book: mustBook(t), DeepSeek: []store.ForkDeepSeekUsageEvent{a, b}, All: all, ProjectID: "p2"})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Totals.Events != 0 || len(scoped.SessionModels) != 1 {
		t.Fatalf("project filter: totals %d, models %v", scoped.Totals.Events, scoped.SessionModels)
	}
}

type fakeReportStore struct {
	events []domain.UsageReportEvent
}

func (f fakeReportStore) ListUsageReportEvents(context.Context, time.Time, time.Time) ([]domain.UsageReportEvent, error) {
	return f.events, nil
}

// pricedRows join the upstream report rows by (period, harness, model,
// speed): every key exists upstream and the token sums agree, for day and
// hour resolution and a non-UTC zone.
func TestPricedRowsJoinUpstreamRows(t *testing.T) {
	now := time.Now().UTC()
	var ds []store.ForkDeepSeekUsageEvent
	var up []domain.UsageReportEvent
	for i, model := range []string{"deepseek-v4-pro", "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-pro"} {
		at := now.Add(-time.Duration(i*5+1) * time.Hour)
		hit, miss, out := int64(10*i), int64(100+i), int64(7)
		route := map[bool]string{true: "deepseek-account", false: "deepseek"}[i%2 == 0]
		e := store.ForkDeepSeekUsageEvent{CreatedAt: at, ProviderID: domain.UsageProviderOpenAI, BillingProviderID: route, ModelID: model,
			CachedTokens: hit, UncachedTokens: miss, OutputTokens: out, Harness: domain.HarnessDeepSeek, SessionID: "s1"}
		ds = append(ds, e)
		up = append(up, domain.UsageReportEvent{CreatedAt: at, ProviderID: domain.UsageProviderOpenAI, BillingProviderID: route, ModelID: model,
			Tokens: domain.UsageTokenMetrics{CachedInputTokens: &hit, UncachedInputTokens: &miss, OutputTokens: &out}, Harness: domain.HarnessDeepSeek, SessionID: "s1"})
	}
	for _, days := range []int{1, 7} {
		for _, tz := range []string{"UTC", "Europe/Berlin", "Asia/Kolkata"} {
			upstream, err := usagesvc.NewReportReader(fakeReportStore{events: up}, nil).Report(context.Background(), days, 0, tz, "")
			if err != nil {
				t.Fatal(err)
			}
			w, err := ReportWindow(context.Background(), days, 0, tz)
			if err != nil {
				t.Fatal(err)
			}
			rep, err := Build(Input{Window: w, Book: mustBook(t), DeepSeek: ds})
			if err != nil {
				t.Fatal(err)
			}
			type key struct{ period, harness, model, speed string }
			want := map[key]int64{}
			for _, r := range upstream.Rows {
				want[key{r.Period, string(r.Harness), r.ModelID, r.Speed}] += r.InputTokens + r.CacheReadTokens + r.CacheWriteTokens + r.OutputTokens
			}
			got := map[key]int64{}
			for _, r := range rep.PricedRows {
				got[key{r.Period, r.Harness, r.Model, r.Speed}] += r.Total()
			}
			if len(got) != len(want) {
				t.Fatalf("days %d tz %s: %d joined keys, upstream has %d (%v vs %v)", days, tz, len(got), len(want), got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Errorf("days %d tz %s: key %+v tokens %d, upstream %d", days, tz, k, got[k], v)
				}
			}
		}
	}
}

func TestReportWindowRejectsBadParameters(t *testing.T) {
	for _, tc := range []struct {
		days, offset int
		tz           string
	}{{0, 0, ""}, {91, 0, ""}, {7, -1, ""}, {7, 0, "Mars/Olympus"}} {
		if _, err := ReportWindow(context.Background(), tc.days, tc.offset, tc.tz); err == nil {
			t.Errorf("%+v accepted", tc)
		}
	}
}

// One model at two speed tiers is one MODELS entry with summed tokens,
// events and cost, also when only the DeepSeek query saw the events.
func TestSessionModelsMergeSpeedTiers(t *testing.T) {
	w := dayWindow(t, "2026-11-23T00:00:00Z", 1)
	a := ev(t, "2026-11-23T12:00:00Z", "deepseek", "deepseek-flash", 0, 1000, 0)
	b := ev(t, "2026-11-23T12:10:00Z", "deepseek", "deepseek-flash", 0, 500, 0)
	b.ServiceTier = "priority"
	rep, err := Build(Input{Window: w, Book: mustBook(t), DeepSeek: []store.ForkDeepSeekUsageEvent{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.SessionRows) != 2 || rep.SessionRows[0].Speed == rep.SessionRows[1].Speed {
		t.Fatalf("session rows = %+v, want one per speed tier", rep.SessionRows)
	}
	models := rep.SessionModels["s1"]
	if len(models) != 1 || models[0].Events != 2 || models[0].Tokens != 1500 || *models[0].CostNanos != 225_000 {
		t.Fatalf("models = %+v", models)
	}
}

// A session model whose DeepSeek events are partly priced keeps the catalog
// cost of the rest ("mixed"); building sessionModels never changes the
// published session rows' costs.
func TestSessionModelsMixedBasisAndRowsUnchanged(t *testing.T) {
	w := dayWindow(t, "2026-11-23T00:00:00Z", 1)
	catalog, other := int64(400), int64(900)
	priced := ev(t, "2026-11-23T12:00:00Z", "deepseek", "deepseek-flash", 0, 1000, 0)
	priced.CatalogCostNanos = &catalog
	proxy := ev(t, "2026-11-23T12:05:00Z", "deepseek-proxy", "deepseek-flash", 0, 10, 0) // unknown route
	tier := ev(t, "2026-11-23T12:10:00Z", "deepseek", "deepseek-flash", 0, 2000, 0)
	tier.ServiceTier = "priority"
	all := []domain.UsageReportEvent{}
	for _, e := range []store.ForkDeepSeekUsageEvent{priced, proxy, tier} {
		hit, miss, out := e.CachedTokens, e.UncachedTokens, e.OutputTokens
		ue := domain.UsageReportEvent{CreatedAt: e.CreatedAt, ModelID: e.ModelID, Harness: e.Harness, SessionID: e.SessionID,
			Tokens: domain.UsageTokenMetrics{CachedInputTokens: &hit, UncachedInputTokens: &miss, OutputTokens: &out}}
		switch e.BillingProviderID {
		case "deepseek-proxy":
			ue.Costs.EstimatedCostNanos = &other
		default:
			ue.Costs.EstimatedCostNanos = e.CatalogCostNanos
		}
		all = append(all, ue)
	}
	rep, err := Build(Input{Window: w, Book: mustBook(t), DeepSeek: []store.ForkDeepSeekUsageEvent{priced, proxy, tier}, All: all})
	if err != nil {
		t.Fatal(err)
	}
	var rowSum int64
	for _, r := range rep.SessionRows {
		rowSum += deref(r.PricebookCostNanos)
	}
	if rowSum != *rep.Totals.PricebookCostNanos || rowSum != 150_000+300_000 {
		t.Fatalf("session rows sum %d, totals %d, want 450000 (rows were mutated?)", rowSum, *rep.Totals.PricebookCostNanos)
	}
	m := rep.SessionModels["s1"]
	// catalog 400 (priced event) + 900 (proxy) - 400 replaced + 450000 price book.
	if len(m) != 1 || m[0].CostBasis != CostBasisMixed || *m[0].CostNanos != 900+450_000 || m[0].Events != 3 {
		t.Fatalf("session models = %+v", m)
	}
}

func TestBookRefStaleForMissingHolidayYear(t *testing.T) {
	st := pricebook.Status{Version: "v", Warnings: []string{}}
	if bookRef(st, nil).Stale {
		t.Fatal("fresh book reported stale")
	}
	if !bookRef(st, []string{pricebook.AssumptionYearMissed}).Stale {
		t.Fatal("a priced event without a holiday calendar did not make the report stale")
	}
}
