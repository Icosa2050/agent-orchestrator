package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type forkReportStore struct {
	events []store.ForkDeepSeekUsageEvent
}

func (s forkReportStore) ListForkDeepSeekUsageEvents(context.Context, time.Time, time.Time, []string) ([]store.ForkDeepSeekUsageEvent, error) {
	return s.events, nil
}

func (forkReportStore) ListUsageReportEvents(context.Context, time.Time, time.Time) ([]domain.UsageReportEvent, error) {
	return nil, nil
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

// runForkReportCLI runs the CLI against a daemon with the real report-ext
// route (ext "": none, an old daemon; "fail": answers 500) and an upstream
// report that answers upstream(groupBy, top).
func runForkReportCLI(t *testing.T, events []store.ForkDeepSeekUsageEvent, ext string, upstream func(groupBy, top string) string, args ...string) (string, *forkReportDaemon, error) {
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
		})
	})
	r.Post("/internal/telemetry/cli-invoked", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/api/v1/usage/report", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstream(req.URL.Query().Get("groupBy"), req.URL.Query().Get("top")))
	})
	switch ext {
	case "ok":
		svc, err := pricebook.NewService(pricebook.Config{DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		reportext.Routes(&reportext.Service{Store: forkReportStore{events: events}, Prices: svc})(r)
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

// forkReportFixture: one costly DeepSeek account-route reply (unpriced in the
// catalog) and a cheap codex row the catalog prices.
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

func forkReportFixture() ([]store.ForkDeepSeekUsageEvent, func(groupBy, top string) string) {
	at := forkFixtureTime()
	period := at.Format("2006-01-02")
	events := []store.ForkDeepSeekUsageEvent{{
		CreatedAt: at, ProviderID: domain.UsageProviderOpenAI, BillingProviderID: "deepseek-account", ModelID: "deepseek-v4-pro",
		UncachedTokens: 1_000_000, Harness: domain.HarnessDeepSeek, SessionID: "ds-1", ProjectID: "p",
	}}
	upstream := func(groupBy, top string) string {
		topField := ""
		if top != "" {
			topField = `"top":` + top + `,`
		}
		if groupBy == "session" {
			rows := `{"sessionId":"cx-1","projectId":"p","harness":"codex","model":"gpt-6","speed":"standard","events":1,"pricedEvents":1,"costNanos":5},` +
				`{"sessionId":"ds-1","projectId":"p","harness":"deepseek-harness","model":"deepseek-v4-pro","speed":"standard","events":1,"pricedEvents":0,"costNanos":0}`
			if top == "1" {
				rows = rows[:strings.Index(rows, "},")+1]
			}
			return `{"days":7,"timeZone":"UTC","rows":[],"sessions":[],` + topField + `"groupBy":"session","sessionRows":[` + rows + `]}`
		}
		rows := `{"period":"` + period + `","harness":"codex","model":"gpt-6","speed":"standard","events":1,"costNanos":5},` +
			`{"period":"` + period + `","harness":"deepseek-harness","model":"deepseek-v4-pro","speed":"standard","events":1,"costNanos":0}`
		if top == "1" {
			rows = rows[:strings.Index(rows, "},")+1]
		}
		return `{"days":7,"timeZone":"UTC",` + topField + `"rows":[` + rows + `],"sessions":[]}`
	}
	return events, upstream
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
	events, upstream := forkReportFixture()
	out, d, err := runForkReportCLI(t, events, "ok", upstream, "usage", "report", "--days", "7")
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
// outranks the codex row upstream kept.
func TestForkUsageReportTopRanksByPricebookCost(t *testing.T) {
	events, upstream := forkReportFixture()
	out, d, err := runForkReportCLI(t, events, "ok", upstream, "usage", "report", "--top", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 3 || got[0] != "/api/v1/usage/report?top=1" || got[2] != "/api/v1/usage/report" {
		t.Fatalf("requests = %v", got)
	}
	m := decodeMerged(t, out)
	if m.Top != 1 || len(m.Rows) != 1 || m.Rows[0].Harness != "deepseek-harness" || m.Ext.RankedBy == "" {
		t.Fatalf("ranked output = %s", out)
	}

	out, _, err = runForkReportCLI(t, events, "ok", upstream, "usage", "report", "--by", "session", "--top", "1")
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
	events, upstream := forkReportFixture()
	out, _, err := runForkReportCLI(t, events, "", upstream, "usage", "report", "--top", "1")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "unsupported" || len(m.Rows) != 1 || m.Rows[0].Harness != "codex" {
		t.Fatalf("old daemon output = %s", out)
	}
	out, _, err = runForkReportCLI(t, events, "fail", upstream, "usage", "report")
	if err != nil {
		t.Fatalf("an ext failure failed the command: %v", err)
	}
	if m := decodeMerged(t, out); m.Ext.Status != "error" || !strings.Contains(m.Ext.Reason, "500") || len(m.Rows) != 2 {
		t.Fatalf("ext error output = %s", out)
	}
}

func TestForkUsageReportByHour(t *testing.T) {
	events, upstream := forkReportFixture()
	out, d, err := runForkReportCLI(t, events, "ok", upstream, "usage", "report", "--by", "hour", "--days", "7", "--tz", "Europe/Berlin")
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

	out, _, err = runForkReportCLI(t, events, "ok", upstream, "usage", "report", "--by", "hour", "--json", "--tz", "Asia/Kolkata")
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

	if _, _, err := runForkReportCLI(t, events, "", upstream, "usage", "report", "--by", "hour"); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Fatalf("old daemon --by hour: %v", err)
	}
	for _, args := range [][]string{{"usage", "report", "--by", "hour", "--top", "2"}, {"usage", "report", "--by", "hour", "--tz", "Mars/Olympus"}} {
		_, d, err := runForkReportCLI(t, events, "ok", upstream, args...)
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
func TestForkUsageReportTopWithoutPricedEventsKeepsUpstream(t *testing.T) {
	_, upstream := forkReportFixture()
	out, d, err := runForkReportCLI(t, nil, "ok", upstream, "usage", "report", "--top", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 2 {
		t.Fatalf("requests = %v, want the upstream report and report-ext only", got)
	}
	m := decodeMerged(t, out)
	if m.Ext.Status != "ok" || m.Ext.RankedBy == "" || len(m.Rows) != 1 || m.Rows[0].Harness != "codex" {
		t.Fatalf("output = %s", out)
	}
}

// Session rows are ranked by effective cost even without --top: the
// DeepSeek session the catalog left at 0 comes first.
func TestForkUsageReportSessionRowsRankedWithoutTop(t *testing.T) {
	events, upstream := forkReportFixture()
	out, _, err := runForkReportCLI(t, events, "ok", upstream, "usage", "report", "--by", "session")
	if err != nil {
		t.Fatal(err)
	}
	m := decodeMerged(t, out)
	if len(m.SessionRows) != 2 || m.SessionRows[0].SessionID != "ds-1" || m.SessionRows[1].SessionID != "cx-1" || m.Top != 0 {
		t.Fatalf("session order = %s", out)
	}
}

// A row mixing price-book-priced DeepSeek events with other catalog-priced
// events keeps the other events' catalog cost in its effective cost.
func TestForkUsageReportEffectiveCostKeepsOtherCatalogCost(t *testing.T) {
	events, _ := forkReportFixture()
	catalog := int64(400)
	events[0].CatalogCostNanos = &catalog
	period := events[0].CreatedAt.Format("2006-01-02")
	upstream := func(string, string) string {
		return `{"days":7,"rows":[{"period":"` + period + `","harness":"deepseek-harness","model":"deepseek-v4-pro","speed":"standard","events":2,"costNanos":1000}],"sessions":[]}`
	}
	out, _, err := runForkReportCLI(t, events, "ok", upstream, "usage", "report")
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
	events, _ := forkReportFixture()
	upstream := func(string, string) string {
		return `{"days":7,"since":"2001-01-01T00:00:00Z","until":"2001-01-08T00:00:00Z","rows":[],"sessions":[]}`
	}
	out, d, err := runForkReportCLI(t, events, "ok", upstream, "usage", "report")
	if err != nil {
		t.Fatal(err)
	}
	if m := decodeMerged(t, out); m.Ext.Status != "error" || !strings.Contains(m.Ext.Reason, "different windows") {
		t.Fatalf("output = %s", out)
	}
	if got := d.seen(); len(got) != 3 {
		t.Fatalf("requests = %v, want one report-ext retry", got)
	}
}
