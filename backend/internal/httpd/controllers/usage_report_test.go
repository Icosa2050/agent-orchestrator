package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
)

type fakeUsageReport struct {
	days, offset int
	tz           string
	projectID    domain.ProjectID
	report       domain.UsageReport
}

func (f *fakeUsageReport) Report(_ context.Context, days, offset int, tz string, projectID domain.ProjectID) (domain.UsageReport, error) {
	f.days, f.offset, f.tz, f.projectID = days, offset, tz, projectID
	return f.report, nil
}

func fixedUsageReport() domain.UsageReport {
	row := func(period, model string, cost int64) domain.UsageReportRow {
		return domain.UsageReportRow{
			Period: period, Harness: domain.HarnessCodex, ModelID: model, Speed: "standard", Events: 2, PricedEvents: 2,
			InputTokens: 100, CacheReadTokens: 900, OutputTokens: 10, CostNanos: cost, InputCostNanos: cost / 2, OutputCostNanos: cost / 2,
		}
	}
	sessionRow := func(session domain.SessionID, project domain.ProjectID, model string, cost int64) domain.UsageReportSessionRow {
		return domain.UsageReportSessionRow{SessionID: session, ProjectID: project, UsageReportRow: row("", model, cost)}
	}
	return domain.UsageReport{
		Days: 2, TimeZone: "UTC", Resolution: "day",
		Since: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
		Periods: []string{"2026-11-23", "2026-11-24"},
		Rows:    []domain.UsageReportRow{row("2026-11-23", "gpt-6.1-sol", 100), row("2026-11-24", "gpt-6.1-sol", 700), row("2026-11-24", "gpt-5.5", 300)},
		Sessions: []domain.UsageReportSession{
			{SessionID: "nebrivo-215", ProjectID: "nebrivo", Harness: domain.HarnessCodex, Title: "Fix import", Events: 6, PricedEvents: 6, Tokens: 3000, CostNanos: 900, LastUsedAt: time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)},
		},
		SessionRows: []domain.UsageReportSessionRow{
			sessionRow("nebrivo-215", "nebrivo", "gpt-6.1-sol", 700),
			sessionRow("nebrivo-203", "nebrivo", "gpt-6.1-sol", 300),
			sessionRow("nebrivo-215", "nebrivo", "gpt-5.5", 100),
		},
	}
}

func getUsageReport(t *testing.T, svc *fakeUsageReport, query string) (string, int) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{UsageReport: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/api/v1/usage/report" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)), resp.StatusCode
}

// The default grouping is read by other tools: the exact bytes must not move.
func TestUsageReportDefaultJSONIsUnchanged(t *testing.T) {
	svc := &fakeUsageReport{report: domain.UsageReport{
		Days: 1, TimeZone: "UTC", Resolution: "hour",
		Since: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC), Periods: []string{"2026-11-24T00:00"},
		Rows: []domain.UsageReportRow{{
			Period: "2026-11-24T00:00", Harness: domain.HarnessCodex, ModelID: "gpt-6.1-sol", Speed: "standard", Events: 2, PricedEvents: 1,
			InputTokens: 100, CacheReadTokens: 900, CacheWriteTokens: 5, OutputTokens: 10,
			InputCostNanos: 4, CacheReadCostNanos: 3, CacheWriteCostNanos: 2, OutputCostNanos: 1, CostNanos: 10, PremiumNanos: 6, CacheSavingsNanos: 7,
		}},
		Sessions: []domain.UsageReportSession{{
			SessionID: "app-1", ProjectID: "app", Harness: domain.HarnessCodex, Title: "Fix", Events: 2, PricedEvents: 1, Tokens: 1010, CostNanos: 10,
			LastUsedAt: time.Date(2026, 11, 24, 0, 30, 0, 0, time.UTC),
		}},
		// Computed for every report; the default response must not show it.
		SessionRows: []domain.UsageReportSessionRow{{SessionID: "app-1", ProjectID: "app", UsageReportRow: domain.UsageReportRow{Harness: domain.HarnessCodex, ModelID: "gpt-6.1-sol", Speed: "standard", CostNanos: 10}}},
	}}
	const want = `{"days":1,"timeZone":"UTC","resolution":"hour","since":"2026-11-24T00:00:00Z","until":"2026-11-24T01:00:00Z","periods":["2026-11-24T00:00"],` +
		`"rows":[{"period":"2026-11-24T00:00","harness":"codex","model":"gpt-6.1-sol","speed":"standard","events":2,"pricedEvents":1,"inputTokens":100,` +
		`"cacheReadTokens":900,"cacheWriteTokens":5,"outputTokens":10,"inputCostNanos":4,"cacheReadCostNanos":3,"cacheWriteCostNanos":2,"outputCostNanos":1,` +
		`"costNanos":10,"premiumNanos":6,"cacheSavingsNanos":7}],` +
		`"sessions":[{"sessionId":"app-1","projectId":"app","harness":"codex","title":"Fix","events":2,"pricedEvents":1,"tokens":1010,"costNanos":10,"lastUsedAt":"2026-11-24T00:30:00Z"}]}`
	for _, query := range []string{"?days=1", "?days=1&groupBy=period"} {
		body, status := getUsageReport(t, svc, query)
		if status != http.StatusOK || body != want {
			t.Fatalf("%s: status %d\n got %s\nwant %s", query, status, body, want)
		}
	}
}

type usageReportBody struct {
	GroupBy     string            `json:"groupBy"`
	Top         int               `json:"top"`
	Rows        []json.RawMessage `json:"rows"`
	Sessions    []json.RawMessage `json:"sessions"`
	SessionRows []struct {
		SessionID string `json:"sessionId"`
		ProjectID string `json:"projectId"`
		Harness   string `json:"harness"`
		Model     string `json:"model"`
		CostNanos int64  `json:"costNanos"`
	} `json:"sessionRows"`
}

func TestUsageReportGroupsBySessionAndKeepsTop(t *testing.T) {
	svc := &fakeUsageReport{report: fixedUsageReport()}
	body, status := getUsageReport(t, svc, "?days=2&offset=1&tz=UTC&projectId=nebrivo&groupBy=session")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if svc.days != 2 || svc.offset != 1 || svc.tz != "UTC" || svc.projectID != "nebrivo" {
		t.Fatalf("window and project not passed on: %+v", svc)
	}
	var got usageReportBody
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.GroupBy != "session" || len(got.Rows) != 0 || len(got.Sessions) != 0 || len(got.SessionRows) != 3 {
		t.Fatalf("session response = %s", body)
	}
	if first := got.SessionRows[0]; first.SessionID != "nebrivo-215" || first.ProjectID != "nebrivo" || first.Harness != "codex" || first.Model != "gpt-6.1-sol" || first.CostNanos != 700 {
		t.Fatalf("first row = %+v", first)
	}
	if strings.Contains(body, `"period"`) || !strings.Contains(body, `"rows":[]`) || !strings.Contains(body, `"sessions":[]`) {
		t.Fatalf("session response should carry no period rows: %s", body)
	}

	body, _ = getUsageReport(t, svc, "?groupBy=session&top=2")
	got = usageReportBody{}
	if err := json.Unmarshal([]byte(body), &got); err != nil || len(got.SessionRows) != 2 || got.SessionRows[1].SessionID != "nebrivo-203" || got.Top != 2 {
		t.Fatalf("top=2 = %s (%v)", body, err)
	}
	body, _ = getUsageReport(t, svc, "?groupBy=session&top=50")
	got = usageReportBody{}
	if err := json.Unmarshal([]byte(body), &got); err != nil || len(got.SessionRows) != 3 {
		t.Fatalf("top larger than the rows = %s (%v)", body, err)
	}
}

func TestUsageReportTopKeepsCostliestPeriodRows(t *testing.T) {
	svc := &fakeUsageReport{report: fixedUsageReport()}
	body, status := getUsageReport(t, svc, "?days=2&top=2")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var got struct {
		Top  int `json:"top"`
		Rows []struct {
			Period    string `json:"period"`
			Model     string `json:"model"`
			CostNanos int64  `json:"costNanos"`
		} `json:"rows"`
		SessionRows []json.RawMessage `json:"sessionRows"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 || got.Rows[0].CostNanos != 700 || got.Rows[1].CostNanos != 300 || got.SessionRows != nil || got.Top != 2 {
		t.Fatalf("top=2 default grouping = %s", body)
	}
	if svc.report.Rows[0].CostNanos != 100 {
		t.Fatalf("top reordered the service's rows in place: %+v", svc.report.Rows)
	}
}

func TestUsageReportRejectsBadGroupingAndTop(t *testing.T) {
	for _, query := range []string{"?groupBy=model", "?top=0", "?top=-3", "?top=many"} {
		svc := &fakeUsageReport{report: fixedUsageReport()}
		body, status := getUsageReport(t, svc, query)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400: %s", query, status, body)
		}
		if svc.days != 0 {
			t.Fatalf("%s: the report was read before the request was rejected", query)
		}
	}
}
