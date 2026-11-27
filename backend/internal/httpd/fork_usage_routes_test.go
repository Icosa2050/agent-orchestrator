package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/providers"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/reportext"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// The fork price-book routes mount through APIDeps.Extensions next to the
// upstream /api/v1/usage routes, and the LAN listener's device-credential
// gate covers them like the rest of usage.
func TestForkUsageRoutesMountBesideUpstreamUsage(t *testing.T) {
	svc, err := pricebook.NewService(pricebook.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{Extensions: []func(chi.Router){pricebook.Routes(svc)}}, ControlDeps{})
	for _, path := range []string{pricebook.RoutePrefix + "/prices", pricebook.RoutePrefix + "/window?at=2026-11-24T02:00:00Z"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["provider"] != "deepseek" {
			t.Fatalf("%s: %s", path, rec.Body)
		}
	}
	for _, path := range []string{"/prices", "/prices/check", "/prices/accept", "/prices/auto", "/window"} {
		if !requiresDeviceCredential(pricebook.RoutePrefix + path) {
			t.Errorf("%s is not behind the LAN device-credential gate", path)
		}
	}
}

// The provider polling routes sit behind the same gate, answer with every
// provider off by default, and refuse the opt-in write from the LAN listener.
func TestForkUsageRoutesProvidersLocalOnlyWrite(t *testing.T) {
	svc, err := providers.NewService(providers.Config{DataDir: t.TempDir(), PAL: providers.PALConfigPaths{ClaudeJSON: filepath.Join(t.TempDir(), "claude.json")}})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{Extensions: []func(chi.Router){providers.Routes(svc)}}, ControlDeps{})
	if !requiresDeviceCredential(providers.RoutePath) {
		t.Fatal("providers route is not behind the LAN device-credential gate")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, providers.RoutePath, nil))
	var body providers.StatusResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Providers) != 2 || body.Providers[0].Status != providers.StatusDisabled {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(http.MethodPost, providers.RoutePath, strings.NewReader(`{"provider":"deepseek","enabled":true}`))
	req = req.WithContext(requestscope.WithLAN(req.Context()))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || svc.Enabled()[providers.DeepSeek] {
		t.Fatalf("LAN POST: %d %s", rec.Code, rec.Body)
	}
}

type forkReportExtStore struct{}

func (forkReportExtStore) ListForkUsageReportSnapshot(context.Context, time.Time, time.Time, []string) ([]store.ForkDeepSeekUsageEvent, []domain.UsageReportEvent, error) {
	return []store.ForkDeepSeekUsageEvent{{CreatedAt: time.Now().UTC().Add(-time.Hour), BillingProviderID: "deepseek-account", ModelID: "deepseek-v4-pro",
		UncachedTokens: 1000, Harness: domain.HarnessDeepSeek, SessionID: "s1"}}, nil, nil
}

// report-ext mounts beside the price book under the device-credential gate,
// validates the upstream report parameters and prices DeepSeek events.
func TestForkUsageRoutesReportExt(t *testing.T) {
	svc, err := pricebook.NewService(pricebook.Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ext := reportext.Routes(&reportext.Service{Store: forkReportExtStore{}, Prices: svc})
	r := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{Extensions: []func(chi.Router){pricebook.Routes(svc), ext}}, ControlDeps{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, reportext.Route+"?days=7&tz=Europe/Berlin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("report-ext: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Schema    int    `json:"schema"`
		TimeZone  string `json:"timeZone"`
		PriceBook *struct {
			Version string `json:"version"`
		} `json:"priceBook"`
		Totals struct {
			Events       int64 `json:"events"`
			PricedEvents int64 `json:"pricedEvents"`
		} `json:"totals"`
		Hours []json.RawMessage `json:"hours"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Schema != 1 || body.TimeZone != "Europe/Berlin" ||
		body.PriceBook == nil || body.Totals.Events != 1 || body.Totals.PricedEvents != 1 || len(body.Hours) != 24 {
		t.Fatalf("report-ext body: %s", rec.Body)
	}
	for _, q := range []string{"?days=x", "?days=0", "?offset=-1", "?tz=Mars/Olympus"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, reportext.Route+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
	if !requiresDeviceCredential(reportext.Route) {
		t.Error("report-ext is not behind the LAN device-credential gate")
	}
}
