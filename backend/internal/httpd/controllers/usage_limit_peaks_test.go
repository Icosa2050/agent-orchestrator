package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type fakeLimitPeaks struct{ weeks int }

func (f *fakeLimitPeaks) Peaks(_ context.Context, weeks int) (domain.UsageLimitPeaks, error) {
	f.weeks = weeks
	if weeks > 26 {
		return domain.UsageLimitPeaks{}, apierr.Invalid("INVALID_USAGE_WINDOW", "weeks must be between 1 and 26", nil)
	}
	reset := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	periods := []domain.UsageLimitPeak{{ResetAt: reset, PeakUsedPct: 82.5, Readings: 900, FirstReadingAt: reset.Add(-7 * 24 * time.Hour), LastReadingAt: reset.Add(-time.Minute)}}
	return domain.UsageLimitPeaks{Windows: []domain.UsageLimitPeakTrack{{
		Provider: domain.AgentAccountProviderCodex, Login: "b@example.com", Email: "b@example.com", AccountID: "device-codex",
		WindowID: "codex:weekly", Label: "Weekly", Periods: periods, Days: []domain.UsageLimitDayPeak{}, Summary: domain.SummarizeLimitPeaks(periods),
	}}}, nil
}

func TestUsageLimitPeaksAPIShape(t *testing.T) {
	peaks := &fakeLimitPeaks{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{UsageLimitPeaks: peaks}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/usage/limit-peaks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || peaks.weeks != 12 {
		t.Fatalf("status %d weeks %d, want 200 and the default 12", resp.StatusCode, peaks.weeks)
	}
	var body struct {
		Windows []struct {
			Provider string `json:"provider"`
			Login    string `json:"login"`
			WindowID string `json:"windowId"`
			Periods  []map[string]any
			Summary  map[string]any
		} `json:"windows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Windows) != 1 || body.Windows[0].Login != "b@example.com" || len(body.Windows[0].Periods) != 1 {
		t.Fatalf("body = %+v", body)
	}
	for _, key := range []string{"resetAt", "peakUsedPct", "readings", "firstReadingAt", "lastReadingAt", "open"} {
		if _, ok := body.Windows[0].Periods[0][key]; !ok {
			t.Fatalf("period lacks %s: %v", key, body.Windows[0].Periods[0])
		}
	}
	for _, key := range []string{"periods", "avgPeak", "maxPeak", "periodsOver80", "periodsUnder30"} {
		if _, ok := body.Windows[0].Summary[key]; !ok {
			t.Fatalf("summary lacks %s: %v", key, body.Windows[0].Summary)
		}
	}
	if body.Windows[0].Summary["periodsOver80"] != float64(1) {
		t.Fatalf("summary = %v", body.Windows[0].Summary)
	}

	for _, tc := range []struct {
		query string
		want  int
	}{{"?weeks=4", http.StatusOK}, {"?weeks=abc", http.StatusBadRequest}, {"?weeks=40", http.StatusBadRequest}} {
		resp, err := http.Get(srv.URL + "/api/v1/usage/limit-peaks" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status %d, want %d", tc.query, resp.StatusCode, tc.want)
		}
	}
}
