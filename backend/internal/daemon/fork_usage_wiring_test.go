package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
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
	usage := startForkUsage(ctx, forkUsageConfig{DataDir: dataDir, FirstDelay: func() time.Duration { return 0 }, Tick: 5 * time.Millisecond})
	waitFor(t, "the price-page check", func() bool { return rt.sawHost("api-docs.deepseek.com") })
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

// With the switch off the daemon never contacts the docs host.
func TestForkPriceCheckSwitchOff(t *testing.T) {
	rt := installRecordingTransport(t)
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(pricebook.ConfigPath(dataDir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pricebook.ConfigPath(dataDir), []byte(`{"pricebook":{"autoCheck":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	usage := startForkUsage(context.Background(), forkUsageConfig{DataDir: dataDir, FirstDelay: func() time.Duration { return 0 }, Tick: time.Millisecond})
	time.Sleep(300 * time.Millisecond)
	usage.stop()
	if got := rt.snapshot(); len(got) != 0 {
		t.Fatalf("requests with the price check off: %+v", got)
	}
	if routes := usage.routes(); len(routes) != 1 {
		t.Fatalf("routes = %d, want the price-book mount", len(routes))
	}
}
