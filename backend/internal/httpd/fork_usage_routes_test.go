package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
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
