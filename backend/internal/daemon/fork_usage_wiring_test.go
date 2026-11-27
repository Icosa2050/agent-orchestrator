package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/providers"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type outboundRequest struct {
	scheme, host, path string
	authorization      bool
	cookie             bool
}

// recordingTransport stands in for the network: it records every outbound
// request and answers locally, the DeepSeek pricing page with the fixture.
type recordingTransport struct {
	mu   sync.Mutex
	reqs []outboundRequest
	page []byte
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, outboundRequest{
		scheme: req.URL.Scheme, host: req.URL.Hostname(), path: req.URL.Path,
		authorization: req.Header.Get("Authorization") != "", cookie: req.Header.Get("Cookie") != "",
	})
	rt.mu.Unlock()
	if req.Body != nil {
		_ = req.Body.Close()
	}
	status, body := http.StatusNotFound, []byte("not found")
	if req.URL.Hostname() == "api-docs.deepseek.com" {
		status, body = http.StatusOK, rt.page
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: http.Header{}, Request: req,
		Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)),
	}, nil
}

func (rt *recordingTransport) snapshot() []outboundRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]outboundRequest(nil), rt.reqs...)
}

func (rt *recordingTransport) sawHost(host string) bool {
	for _, r := range rt.snapshot() {
		if r.host == host {
			return true
		}
	}
	return false
}

// installRecordingTransport swaps http.DefaultTransport, which both the
// LiteLLM fetcher and the price-page check use by default. Not parallel.
func installRecordingTransport(t *testing.T) *recordingTransport {
	t.Helper()
	page, err := os.ReadFile(filepath.Join("..", "forkusage", "pricebook", "testdata", "pricing-2026-11-24.md"))
	if err != nil {
		t.Fatal(err)
	}
	rt := &recordingTransport{page: page}
	saved := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = saved })
	return rt
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With default configuration the daemon's background price fetches reach
// only the expected hosts (FORK.md "Default outbound endpoints"), over HTTPS,
// with no credentials or cookies. A new default destination must be added to
// FORK.md and to this list.
func TestForkDefaultOutboundHosts(t *testing.T) {
	rt := installRecordingTransport(t)
	allowed := map[string]string{
		"raw.githubusercontent.com": "/BerriAI/litellm/main/model_prices_and_context_window.json",
		"api-docs.deepseek.com":     "/quick_start/pricing.md",
		"openrouter.ai":             "/api/v1/models", // keyless model listing only, never /api/v1/key
	}
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime, err := newUsagePricingRuntime(usagePricingRuntimeConfig{DataDir: dataDir, Store: sqlitetest.MustOpenAt(t, dataDir), Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	usage := startForkUsage(ctx, forkUsageConfig{DataDir: dataDir, FirstDelay: func() time.Duration { return 0 }, Tick: 5 * time.Millisecond,
		Keys: forkTestKeys{}, ProviderTick: 5 * time.Millisecond})
	waitFor(t, "the price-page check", func() bool { return rt.sawHost("api-docs.deepseek.com") })
	waitFor(t, "the OpenRouter model listing", func() bool { return rt.sawHost("openrouter.ai") })
	waitFor(t, "the LiteLLM price table", func() bool { return rt.sawHost("raw.githubusercontent.com") })
	usage.stop()
	cancel()
	runtime.Wait()
	for _, r := range rt.snapshot() {
		want, ok := allowed[r.host]
		if !ok {
			t.Errorf("unexpected default outbound host %s%s", r.host, r.path)
			continue
		}
		if r.scheme != "https" || r.path != want || r.authorization || r.cookie {
			t.Errorf("request %+v: want https %s with no credentials", r, want)
		}
	}
	if st := usage.svc.Status(); st.LastCheckStatus != pricebook.OutcomeUnchanged {
		t.Errorf("price check through the daemon wiring: %+v", st)
	}
}

// With the switches off the daemon never contacts the docs host or the
// OpenRouter model listing.
func TestForkPriceCheckSwitchOff(t *testing.T) {
	rt := installRecordingTransport(t)
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(pricebook.ConfigPath(dataDir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pricebook.ConfigPath(dataDir), []byte(`{"pricebook":{"autoCheck":false},"openrouterModels":{"autoFetch":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	usage := startForkUsage(context.Background(), forkUsageConfig{DataDir: dataDir, FirstDelay: func() time.Duration { return 0 }, Tick: time.Millisecond,
		ProviderTick: time.Millisecond})
	time.Sleep(300 * time.Millisecond)
	usage.stop()
	if got := rt.snapshot(); len(got) != 0 {
		t.Fatalf("requests with the price check off: %+v", got)
	}
	if routes := usage.routes(); len(routes) != 2 {
		t.Fatalf("routes = %d, want the price-book and providers mounts", len(routes))
	}
}

type forkTestKeys struct{}

func (forkTestKeys) Key(context.Context, string) (string, error) {
	return "sk-SENTINEL-daemon-provider-usage", nil
}

// With default configuration (every provider off) the daemon sends no keyed
// request to api.deepseek.com or openrouter.ai, neither from the poll loop nor
// when the providers route is read, even with keys available.
func TestForkProviderUsageOffByDefault(t *testing.T) {
	rt := installRecordingTransport(t)
	dataDir := t.TempDir()
	usage := startForkUsage(context.Background(), forkUsageConfig{
		DataDir: dataDir, Keys: forkTestKeys{}, FirstDelay: func() time.Duration { return time.Hour }, ProviderTick: time.Millisecond,
	})
	r := chi.NewRouter()
	for _, mount := range usage.routes() {
		mount(r)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, providers.RoutePath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"disabled"`) {
		t.Fatalf("providers route: %d %s", rec.Code, rec.Body)
	}
	time.Sleep(200 * time.Millisecond)
	usage.stop()
	for _, req := range rt.snapshot() {
		// The keyless model listing is the only default openrouter.ai request.
		keyless := req.host == "openrouter.ai" && req.path == "/api/v1/models" && !req.authorization && !req.cookie
		if req.host == "api.deepseek.com" || (req.host == "openrouter.ai" && !keyless) || req.authorization {
			t.Errorf("request without opt-in: %+v", req)
		}
	}
	// Opting in reaches only the provider's own host, with the key.
	if err := usage.providers.SetEnabled(providers.DeepSeek, true); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, providers.RoutePath, nil))
	if !rt.sawHost("api.deepseek.com") {
		t.Fatal("enabled DeepSeek polling sent nothing")
	}
	for _, req := range rt.snapshot() {
		if req.host == "api.deepseek.com" && (req.path != "/user/balance" || !req.authorization || req.scheme != "https") {
			t.Errorf("balance request %+v", req)
		}
		if req.host == "openrouter.ai" && (req.path != "/api/v1/models" || req.authorization) {
			t.Errorf("openrouter key contacted while off: %+v", req)
		}
	}
	if strings.Contains(rec.Body.String(), "SENTINEL") {
		t.Fatalf("key in response: %s", rec.Body)
	}
}

// With the store wired, the daemon mounts report-ext next to the price book.
func TestForkUsageWiringMountsReportExt(t *testing.T) {
	installRecordingTransport(t)
	usage := startForkUsage(context.Background(), forkUsageConfig{DataDir: t.TempDir(), Store: sqlitetest.MustOpen(t),
		FirstDelay: func() time.Duration { return time.Hour }})
	defer usage.stop()
	if routes := usage.routes(); len(routes) != 3 {
		t.Fatalf("routes = %d, want the price book, report-ext and providers", len(routes))
	}
}
