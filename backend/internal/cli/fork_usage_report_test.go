package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/reportext"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// forkReportState is the fake daemon's usage database: the upstream report
// (the real upstream reader and controller) and report-ext both read it.
// afterUpstream runs after the n-th upstream report read, to ingest events
// between the CLI's reads.
type forkReportState struct {
	mu            sync.Mutex
	deepseek      []store.ForkDeepSeekUsageEvent
	all           []domain.UsageReportEvent
	upstreamReads int
	afterUpstream func(n int, s *forkReportState)
}

// add ingests one event; deepseek events are also in the fork query.
func (s *forkReportState) add(e store.ForkDeepSeekUsageEvent, deepseek bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hit, miss, out := e.CachedTokens, e.UncachedTokens, e.OutputTokens
	s.all = append(s.all, domain.UsageReportEvent{
		CreatedAt: e.CreatedAt, ProviderID: e.ProviderID, BillingProviderID: e.BillingProviderID, ModelID: e.ModelID, ServiceTier: e.ServiceTier,
		Tokens: domain.UsageTokenMetrics{CachedInputTokens: &hit, UncachedInputTokens: &miss, OutputTokens: &out},
		Costs:  domain.UsageEventCosts{EstimatedCostNanos: e.CatalogCostNanos}, Harness: e.Harness, SessionID: e.SessionID, ProjectID: e.ProjectID,
	})
	if deepseek {
		s.deepseek = append(s.deepseek, e)
	}
}

func (s *forkReportState) ListUsageReportEvents(context.Context, time.Time, time.Time) ([]domain.UsageReportEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.UsageReportEvent(nil), s.all...), nil
}

func (s *forkReportState) ListForkUsageReportSnapshot(context.Context, time.Time, time.Time, []string) ([]store.ForkDeepSeekUsageEvent, []domain.UsageReportEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.ForkDeepSeekUsageEvent(nil), s.deepseek...), append([]domain.UsageReportEvent(nil), s.all...), nil
}

type forkReportDaemon struct {
	mu       sync.Mutex
	requests []string
}

func (d *forkReportDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

// runForkReportCLI runs the CLI against a daemon serving the real upstream
// report and (ext "ok") the real report-ext over state; ext "" is an old
// daemon, "fail" answers report-ext with 500. override, when set, replaces
// the upstream report's answer.
func runForkReportCLI(t *testing.T, state *forkReportState, ext string, override func(groupBy, top string) string, args ...string) (string, *forkReportDaemon, error) {
	t.Helper()
	cfg := setConfigEnv(t)
	d := &forkReportDaemon{}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path != "/internal/telemetry/cli-invoked" {
				d.mu.Lock()
				d.requests = append(d.requests, req.URL.RequestURI())
				d.mu.Unlock()
			}
			next.ServeHTTP(w, req)
			if req.URL.Path == "/api/v1/usage/report" {
				state.mu.Lock()
				state.upstreamReads++
				n, hook := state.upstreamReads, state.afterUpstream
				state.mu.Unlock()
				if hook != nil {
					hook(n, state)
				}
			}
		})
	})
	r.Post("/internal/telemetry/cli-invoked", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if override != nil {
		r.Get("/api/v1/usage/report", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, override(req.URL.Query().Get("groupBy"), req.URL.Query().Get("top")))
		})
	} else {
		usage := &controllers.UsageController{Report: usagesvc.NewReportReader(state, nil), Log: slog.Default()}
		r.Route("/api/v1", usage.Register)
	}
	switch ext {
	case "ok":
		svc, err := pricebook.NewService(pricebook.Config{DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		reportext.Routes(&reportext.Service{Store: state, Prices: svc})(r)
	case "fail":
		r.Get("/api/v1/usage/fork/report-ext", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom","code":"INTERNAL"}}`)
		})
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
	if err != nil {
		t.Logf("stderr: %s", errOut)
	}
	return out, d, err
}

// forkFixtureTime is the latest weekend 00:30 UTC before now: always
// off-peak and inside the default 7-day window, whatever the wall clock.
func forkFixtureTime() time.Time {
	now := time.Now().UTC()
	for back := 0; ; back++ {
		d := now.AddDate(0, 0, -back)
		at := time.Date(d.Year(), d.Month(), d.Day(), 0, 30, 0, 0, time.UTC)
		if (at.Weekday() == time.Saturday || at.Weekday() == time.Sunday) && at.Before(now) {
			return at
		}
	}
}

// forkDeepSeekReply is a DeepSeek account-route reply of 1M cache-miss
// tokens the catalog leaves unpriced: $0.66 off-peak in the price book.
func forkDeepSeekReply(minute int) store.ForkDeepSeekUsageEvent {
	return store.ForkDeepSeekUsageEvent{
		CreatedAt: forkFixtureTime().Add(time.Duration(minute) * time.Minute), ProviderID: domain.UsageProviderOpenAI,
		BillingProviderID: "deepseek-account", ModelID: "deepseek-v4-pro", UncachedTokens: 1_000_000,
		Harness: domain.HarnessDeepSeek, SessionID: "ds-1", ProjectID: "p",
	}
}

// forkReportFixture: one costly DeepSeek reply and a cheap codex reply the
// catalog prices at 5 nano-USD.
func forkReportFixture() *forkReportState {
	s := &forkReportState{}
	s.add(forkDeepSeekReply(0), true)
	cost := int64(5)
	s.add(store.ForkDeepSeekUsageEvent{CreatedAt: forkFixtureTime(), ProviderID: domain.UsageProviderOpenAI, BillingProviderID: "openai",
		ModelID: "gpt-6", UncachedTokens: 10, CatalogCostNanos: &cost, Harness: domain.HarnessCodex, SessionID: "cx-1", ProjectID: "p"}, false)
	return s
}

type forkMerged struct {
	Top  int `json:"top"`
	Rows []struct {
		Harness string      `json:"harness"`
		Ext     *forkRowExt `json:"ext"`
	} `json:"rows"`
	SessionRows []struct {
		SessionID string      `json:"sessionId"`
		Ext       *forkRowExt `json:"ext"`
	} `json:"sessionRows"`
	Ext struct {
		Status   string `json:"status"`
		Reason   string `json:"reason"`
		RankedBy string `json:"rankedBy"`
		Totals   struct {
			PricebookCostNanos *int64 `json:"pricebookCostNanos"`
		} `json:"totals"`
		SessionModels map[string][]struct {
			Model     string `json:"model"`
			CostBasis string `json:"costBasis"`
		} `json:"sessionModels"`
	} `json:"ext"`
}

func decodeMerged(t *testing.T, out string) forkMerged {
	t.Helper()
	var m forkMerged
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return m
}

func TestForkUsageReportMergesExtIntoRows(t *testing.T) {
	out, d, err := runForkReportCLI(t, forkReportFixture(), "ok", nil, "usage", "report", "--days", "7")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 2 || got[0] != "/api/v1/usage/report?days=7" || got[1] != "/api/v1/usage/fork/report-ext?days=7" {
		t.Fatalf("requests = %v", got)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "ok" || m.Ext.Totals.PricebookCostNanos == nil || *m.Ext.Totals.PricebookCostNanos != 660_000_000 {
		t.Fatalf("ext = %+v\n%s", m.Ext, out)
	}
	if len(m.Rows) != 2 || m.Rows[0].Ext != nil || m.Rows[1].Ext == nil || *m.Rows[1].Ext.PricebookCostNanos != 660_000_000 ||
		m.Rows[1].Ext.BilledAs != "deepseek-v4-pro" || len(m.Rows[1].Ext.Routes) != 1 || m.Rows[1].Ext.Routes[0] != "deepseek-account" {
		t.Fatalf("rows = %s", out)
	}
	if !strings.Contains(out, `"costNanos": 0`) {
		t.Fatalf("upstream row fields were not kept:\n%s", out)
	}
}

// --top ranks by price-book cost: the DeepSeek row the catalog left at 0
// outranks the codex row upstream kept. The CLI re-reads the report without
// top, then report-ext again so the extension is not older than the report.
func TestForkUsageReportTopRanksByPricebookCost(t *testing.T) {
	out, d, err := runForkReportCLI(t, forkReportFixture(), "ok", nil, "usage", "report", "--top", "1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/v1/usage/report?top=1", "/api/v1/usage/fork/report-ext", "/api/v1/usage/report", "/api/v1/usage/fork/report-ext"}
	if got := d.seen(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "ok" || m.Top != 1 || len(m.Rows) != 1 || m.Rows[0].Harness != "deepseek-harness" || m.Ext.RankedBy == "" {
		t.Fatalf("ranked output = %s", out)
	}

	out, _, err = runForkReportCLI(t, forkReportFixture(), "ok", nil, "usage", "report", "--by", "session", "--top", "1")
	if err != nil {
		t.Fatal(err)
	}
	m = decodeMerged(t, out)
	if len(m.SessionRows) != 1 || m.SessionRows[0].SessionID != "ds-1" || m.SessionRows[0].Ext == nil ||
		*m.SessionRows[0].Ext.PricebookCostNanos != 660_000_000 || m.SessionRows[0].Ext.BillingStatuses["priced"] != 1 {
		t.Fatalf("session ranked output = %s", out)
	}
	if models := m.Ext.SessionModels["ds-1"]; len(models) != 1 || models[0].CostBasis != "pricebook" {
		t.Fatalf("sessionModels = %+v", m.Ext.SessionModels)
	}
}

func TestForkUsageReportExtUnsupportedAndError(t *testing.T) {
	out, _, err := runForkReportCLI(t, forkReportFixture(), "", nil, "usage", "report", "--top", "1")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "unsupported" || len(m.Rows) != 1 || m.Rows[0].Harness != "codex" {
		t.Fatalf("old daemon output = %s", out)
	}
	out, _, err = runForkReportCLI(t, forkReportFixture(), "fail", nil, "usage", "report")
	if err != nil {
		t.Fatalf("an ext failure failed the command: %v", err)
	}
	if m := decodeMerged(t, out); m.Ext.Status != "error" || !strings.Contains(m.Ext.Reason, "500") || len(m.Rows) != 2 {
		t.Fatalf("ext error output = %s", out)
	}
}

func TestForkUsageReportByHour(t *testing.T) {
	state := forkReportFixture()
	events := state.deepseek
	out, d, err := runForkReportCLI(t, state, "ok", nil, "usage", "report", "--by", "hour", "--days", "7", "--tz", "Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 1 || got[0] != "/api/v1/usage/fork/report-ext?days=7" {
		t.Fatalf("requests = %v, want report-ext only and no tz (windows stay UTC)", got)
	}
	hour := events[0].CreatedAt.Format("15:00")
	for _, want := range []string{"DeepSeek usage by hour of day (UTC)", "HOUR UTC", "EUROPE/BERLIN", "\n" + hour + " ", "01:00", "* peak hour: 01:00-04:00, 06:00-10:00 UTC on Mon,Tue,Wed,Thu,Fri except Chinese public holidays",
		"TOTAL", "Europe/Berlin times use the offset on", "prices: deepseek-"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if lines := strings.Count(out, "\n"); lines < 27 {
		t.Errorf("histogram has %d lines, want 24 hours plus header and footer:\n%s", lines, out)
	}

	out, _, err = runForkReportCLI(t, state, "ok", nil, "usage", "report", "--by", "hour", "--json", "--tz", "Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		By              string `json:"by"`
		WindowsTimeZone string `json:"windowsTimeZone"`
		DisplayTimeZone string `json:"displayTimeZone"`
		Hours           []struct {
			Hour     int    `json:"hour"`
			UTC      string `json:"utc"`
			Display  string `json:"display"`
			PeakHour bool   `json:"peakHour"`
			Events   int64  `json:"events"`
		} `json:"hours"`
		Totals struct {
			Events int64 `json:"events"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if v.By != "hour" || v.WindowsTimeZone != "UTC" || v.DisplayTimeZone != "Asia/Kolkata" || len(v.Hours) != 24 || v.Totals.Events != 1 {
		t.Fatalf("hour JSON = %s", out)
	}
	if h := v.Hours[0]; h.Hour != 0 || h.UTC != "00:00" || h.Display != "05:30" || h.PeakHour {
		t.Fatalf("hour 0 = %+v", h)
	}
	if !v.Hours[1].PeakHour || v.Hours[events[0].CreatedAt.Hour()].Events != 1 {
		t.Fatalf("hours = %+v", v.Hours)
	}

	if _, _, err := runForkReportCLI(t, state, "", nil, "usage", "report", "--by", "hour"); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Fatalf("old daemon --by hour: %v", err)
	}
	for _, args := range [][]string{{"usage", "report", "--by", "hour", "--top", "2"}, {"usage", "report", "--by", "hour", "--tz", "Mars/Olympus"}} {
		_, d, err := runForkReportCLI(t, state, "ok", nil, args...)
		var usage usageError
		if !errors.As(err, &usage) || len(d.seen()) != 0 {
			t.Fatalf("%v: err %v requests %v, want a usage error before any request", args, err, d.seen())
		}
	}
}

// --fleet fetches report-ext from each host itself; a host without it keeps
// its upstream answer with ext "unsupported".
func TestForkUsageReportFleetExtPerHost(t *testing.T) {
	env := newRemoteTestEnv(t)
	good := newFakeRemoteHost(t, "host-good")
	env.add(t, "good", good)
	out, err := env.run(t, "usage", "report", "--fleet", "--by", "session")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Hosts []struct {
			Name   string
			Status string
			Data   *struct {
				GroupBy string `json:"groupBy"`
				Ext     struct {
					Status string `json:"status"`
				} `json:"ext"`
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	found := false
	for _, h := range res.Hosts {
		if h.Name == "good" {
			found = true
			if h.Status != remoteStatusOK || h.Data == nil || h.Data.GroupBy != "session" || h.Data.Ext.Status != "unsupported" {
				t.Fatalf("good host = %s", out)
			}
		}
	}
	seenExt := false
	for _, r := range good.seen() {
		seenExt = seenExt || strings.HasPrefix(r, "GET /api/v1/usage/fork/report-ext")
	}
	if !found || !seenExt {
		t.Fatalf("good host missing or report-ext not read from it: %v", good.seen())
	}
}

// --top with no priced DeepSeek events keeps upstream's ranking and makes no
// second upstream read.
// --top with no priced DeepSeek events keeps upstream's ranking and makes no
// second upstream read.
func TestForkUsageReportTopWithoutPricedEventsKeepsUpstream(t *testing.T) {
	state := &forkReportState{}
	cost := int64(5)
	state.add(store.ForkDeepSeekUsageEvent{CreatedAt: forkFixtureTime(), ProviderID: domain.UsageProviderOpenAI, BillingProviderID: "openai",
		ModelID: "gpt-6", UncachedTokens: 10, CatalogCostNanos: &cost, Harness: domain.HarnessCodex, SessionID: "cx-1"}, false)
	out, d, err := runForkReportCLI(t, state, "ok", nil, "usage", "report", "--top", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 2 {
		t.Fatalf("requests = %v, want the upstream report and report-ext only", got)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "ok" || len(m.Rows) != 1 || m.Rows[0].Harness != "codex" {
		t.Fatalf("output = %s", out)
	}
}

// Session rows are ranked by effective cost even without --top: the
// DeepSeek session the catalog left at 0 comes first.
func TestForkUsageReportSessionRowsRankedWithoutTop(t *testing.T) {
	out, _, err := runForkReportCLI(t, forkReportFixture(), "ok", nil, "usage", "report", "--by", "session")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "ok" || len(m.SessionRows) != 2 || m.SessionRows[0].SessionID != "ds-1" || m.SessionRows[1].SessionID != "cx-1" || m.Top != 0 {
		t.Fatalf("session order = %s", out)
	}
}

// A row mixing price-book-priced DeepSeek events with other catalog-priced
// events keeps the other events' catalog cost in its effective cost.
func TestForkUsageReportEffectiveCostKeepsOtherCatalogCost(t *testing.T) {
	state := &forkReportState{}
	priced, other := forkDeepSeekReply(0), forkDeepSeekReply(1)
	catalog, otherCost := int64(400), int64(600)
	priced.CatalogCostNanos = &catalog
	other.BillingProviderID, other.CatalogCostNanos = "openrouter", &otherCost // same row, not in the fork query
	state.add(priced, true)
	state.add(other, false)
	out, _, err := runForkReportCLI(t, state, "ok", nil, "usage", "report")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMerged(t, out)
	if len(m.Rows) != 1 || m.Rows[0].Ext == nil || m.Rows[0].Ext.EffectiveCostNanos == nil ||
		*m.Rows[0].Ext.EffectiveCostNanos != 1000-400+660_000_000 || m.Rows[0].Ext.ReplacedCatalogCostNanos != 400 {
		t.Fatalf("effective cost = %s", out)
	}
}

// An upstream report over another window than report-ext (a period
// rollover between the reads) is not merged.
func TestForkUsageReportWindowMismatchIsAnExtError(t *testing.T) {
	override := func(string, string) string {
		return `{"days":7,"since":"2001-01-01T00:00:00Z","until":"2001-01-08T00:00:00Z","rows":[],"sessions":[]}`
	}
	out, d, err := runForkReportCLI(t, forkReportFixture(), "ok", override, "usage", "report")
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeMerged(t, out); m.Ext.Status != "error" || !strings.Contains(m.Ext.Reason, "different windows") {
		t.Fatalf("output = %s", out)
	}
	if got := d.seen(); len(got) != 2*forkConsistencyAttempts {
		t.Fatalf("requests = %v, want %d report/report-ext pairs", got, forkConsistencyAttempts)
	}
}

// Ingestion between the report and report-ext reads: the extension's
// snapshot shows replies the report did not count. The CLI re-reads both
// and merges only the consistent pair (no understated or negative cost).
func TestForkUsageReportIngestionBetweenReadsRetries(t *testing.T) {
	state := forkReportFixture()
	state.afterUpstream = func(n int, s *forkReportState) {
		if n == 1 {
			s.add(forkDeepSeekReply(1), true)
			s.add(forkDeepSeekReply(2), true)
		}
	}
	out, d, err := runForkReportCLI(t, state, "ok", nil, "usage", "report")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 4 {
		t.Fatalf("requests = %v, want one re-read of both", got)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "ok" || len(m.Rows) != 2 || m.Rows[1].Ext == nil || m.Rows[1].Ext.Events != 3 ||
		*m.Rows[1].Ext.EffectiveCostNanos != 3*660_000_000 {
		t.Fatalf("merged output = %s", out)
	}
}

// The --top re-read without top happens after the first report-ext read;
// ingestion right after it must not be merged against the older extension.
func TestForkUsageReportIngestionDuringTopRereadRetries(t *testing.T) {
	state := forkReportFixture()
	state.afterUpstream = func(n int, s *forkReportState) {
		if n == 2 {
			s.add(forkDeepSeekReply(1), true)
		}
	}
	out, d, err := runForkReportCLI(t, state, "ok", nil, "usage", "report", "--top", "1")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "ok" || m.Top != 1 || len(m.Rows) != 1 || m.Rows[0].Ext == nil || m.Rows[0].Ext.Events != 2 ||
		*m.Rows[0].Ext.EffectiveCostNanos != 2*660_000_000 {
		t.Fatalf("merged output = %s\nrequests %v", out, d.seen())
	}
	for _, r := range d.seen()[2:] {
		if strings.Contains(r, "top=") {
			t.Fatalf("a re-read kept top: %v", d.seen())
		}
	}
}

// Events that keep changing: after forkConsistencyAttempts the CLI gives up
// with ext.status "error" and the upstream answer unchanged, never a wrong
// merge.
func TestForkUsageReportIngestionThatNeverSettlesIsAnExtError(t *testing.T) {
	state := forkReportFixture()
	state.afterUpstream = func(n int, s *forkReportState) { s.add(forkDeepSeekReply(n), true) }
	out, _, err := runForkReportCLI(t, state, "ok", nil, "usage", "report", "--by", "session")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "error" || !strings.Contains(m.Ext.Reason, "changed between") {
		t.Fatalf("output = %s", out)
	}
	for _, r := range m.SessionRows {
		if r.Ext != nil {
			t.Fatalf("a row was merged against a changed extension: %s", out)
		}
	}
}

// Every --tz label uses the offset at the window's last instant, also when
// that date holds a DST switch (Europe/Berlin: 2026-03-29 spring forward,
// 2026-10-25 fall back).
func TestForkUsageReportByHourDisplayUsesOneOffset(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	hours := make([]forkHour, 24)
	for h := range hours {
		hours[h] = forkHour{Hour: h, UTC: time.Date(2000, 1, 1, h, 0, 0, 0, time.UTC).Format("15:04")}
	}
	for _, tc := range []struct {
		until, offset string
		want          map[int]string
	}{
		// Fall back: the last instant 2026-10-25T23:59:59Z is CET (+01:00).
		{"2026-10-26T00:00:00Z", "+01:00", map[int]string{0: "01:00", 1: "02:00", 2: "03:00", 23: "00:00"}},
		// Spring forward: the last instant 2026-03-29T23:59:59Z is CEST (+02:00).
		{"2026-03-30T00:00:00Z", "+02:00", map[int]string{0: "02:00", 1: "03:00", 2: "04:00", 22: "00:00", 23: "01:00"}},
	} {
		until, err := time.Parse(time.RFC3339, tc.until)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(map[string]any{"days": 1, "since": until.Add(-24 * time.Hour), "until": until, "hours": hours})
		if err != nil {
			t.Fatal(err)
		}
		view, err := forkHoursView(raw, "Europe/Berlin", berlin)
		if err != nil {
			t.Fatal(err)
		}
		if view.DisplayUTCOffset != tc.offset {
			t.Errorf("until %s: offset %q, want %q", tc.until, view.DisplayUTCOffset, tc.offset)
		}
		seen := map[string]bool{}
		for _, h := range view.Hours {
			if seen[h.Display] {
				t.Errorf("until %s: label %s used twice (mixed offsets)", tc.until, h.Display)
			}
			seen[h.Display] = true
			if want, ok := tc.want[h.Hour]; ok && h.Display != want {
				t.Errorf("until %s: hour %02d shows %s, want %s", tc.until, h.Hour, h.Display, want)
			}
		}
	}
}
