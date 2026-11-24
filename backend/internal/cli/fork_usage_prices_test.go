package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
)

type forkPricePage struct {
	mu   sync.Mutex
	body string
	srv  *httptest.Server
}

func (p *forkPricePage) set(body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.body = body
}

func newForkPriceService(t *testing.T) (*pricebook.Service, *forkPricePage) {
	t.Helper()
	fixture, err := os.ReadFile("../forkusage/pricebook/testdata/pricing-2026-11-24.md")
	if err != nil {
		t.Fatal(err)
	}
	page := &forkPricePage{body: string(fixture)}
	page.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page.mu.Lock()
		defer page.mu.Unlock()
		_, _ = w.Write([]byte(page.body))
	}))
	t.Cleanup(page.srv.Close)
	svc, err := pricebook.NewService(pricebook.Config{DataDir: t.TempDir(), Client: page.srv.Client(), URL: page.srv.URL + "/pricing.md"})
	if err != nil {
		t.Fatal(err)
	}
	return svc, page
}

// runForkUsageCLI runs the CLI against a daemon that serves the real fork
// price-book routes (svc nil: an old daemon without them).
func runForkUsageCLI(t *testing.T, svc *pricebook.Service, args ...string) (string, error) {
	t.Helper()
	cfg := setConfigEnv(t)
	r := chi.NewRouter()
	r.Post("/internal/telemetry/cli-invoked", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if svc != nil {
		pricebook.Routes(svc)(r)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	out, errOut, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
	if err != nil {
		t.Logf("stderr: %s", errOut)
	}
	return out, err
}

func TestForkUsageWindowText(t *testing.T) {
	svc, _ := newForkPriceService(t)
	out, err := runForkUsageCLI(t, svc, "usage", "window", "--at", "2026-11-27T09:59:00Z")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"deepseek: PEAK at Fri 27 Nov 09:59 UTC (10:59 Berlin)",
		"until          Fri 27 Nov 10:00 UTC (11:00 Berlin)",
		"next off-peak  Fri 27 Nov 10:00 UTC (11:00 Berlin) -> Mon 30 Nov 01:00 UTC (02:00 Berlin)",
		"next peak      Mon 30 Nov 01:00 UTC (02:00 Berlin) -> Mon 30 Nov 04:00 UTC (05:00 Berlin)",
		"holidays: 2026; prices: deepseek-2026-09-10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Summer time shifts only the Berlin display.
	out, err = runForkUsageCLI(t, svc, "usage", "window", "deepseek", "--at", "2026-07-01T05:00:00+02:00")
	if err != nil || !strings.Contains(out, "until          Wed 01 Jul 04:00 UTC (06:00 Berlin)") {
		t.Errorf("summer: %v\n%s", err, out)
	}
}

func TestForkUsageWindowNextAndJSON(t *testing.T) {
	svc, _ := newForkPriceService(t)
	for _, c := range []struct{ at, next, want string }{
		{"2026-11-27T09:59:00Z", "offpeak", "2026-11-27T10:00:00Z"},
		{"2026-11-28T12:00:00Z", "off-peak", "2026-11-28T12:00:00Z"}, // already off-peak: now
		{"2026-11-28T12:00:00Z", "peak", "2026-11-30T01:00:00Z"},
		{"2026-09-30T12:00:00Z", "peak", "2026-10-08T01:00:00Z"}, // across National Day
	} {
		out, err := runForkUsageCLI(t, svc, "usage", "window", "--at", c.at, "--next", c.next)
		if err != nil || strings.TrimSpace(out) != c.want {
			t.Errorf("%s --next %s = %q (%v), want %s", c.at, c.next, out, err, c.want)
		}
	}
	out, err := runForkUsageCLI(t, svc, "usage", "window", "--at", "2026-11-24T02:00:00Z", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep["window"] != "peak" || rep["peakVsOffPeak"] != "2" || rep["windowEndsAt"] == nil || rep["nextOffPeak"] == nil || len(rep["models"].([]any)) != 2 {
		t.Errorf("json: %s", out)
	}
	if _, err := runForkUsageCLI(t, svc, "usage", "window", "--next", "cheap"); ExitCode(err) != 2 {
		t.Errorf("bad --next: %v", err)
	}
	if _, err := runForkUsageCLI(t, svc, "usage", "window", "--at", "tomorrow"); ExitCode(err) != 2 {
		t.Errorf("bad --at: %v", err)
	}
}

func TestForkUsagePricesCheckExitCodes(t *testing.T) {
	svc, page := newForkPriceService(t)
	out, err := runForkUsageCLI(t, svc, "usage", "prices", "--check")
	if err != nil || !strings.Contains(out, "check: unchanged") {
		t.Fatalf("unchanged: %v\n%s", err, out)
	}
	page.set("<html><body>gone</body></html>")
	out, err = runForkUsageCLI(t, svc, "usage", "prices", "deepseek", "--check")
	if ExitCode(err) != exitPricesCannotVerify || !strings.Contains(out, "HTML instead of Markdown") {
		t.Fatalf("cannot verify: exit %d %v\n%s", ExitCode(err), err, out)
	}
	fixture, _ := os.ReadFile("../forkusage/pricebook/testdata/pricing-2026-11-24.md")
	page.set(strings.Replace(string(fixture), "| $3.96 ", "| $3.5  ", 1))
	out, err = runForkUsageCLI(t, svc, "usage", "prices", "--check", "--json")
	if ExitCode(err) != exitPricesChanged {
		t.Fatalf("changed: exit %d %v\n%s", ExitCode(err), err, out)
	}
	var res forkCheckResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Recorded == "" || res.Status.Version != res.Recorded {
		t.Fatalf("changed json: %v\n%s", err, out)
	}
	out, err = runForkUsageCLI(t, svc, "usage", "prices")
	if err != nil || !strings.Contains(out, "prices: "+res.Recorded+" (auto-updated from page") || !strings.Contains(out, "3.5") {
		t.Errorf("status after change: %v\n%s", err, out)
	}
	if _, err := runForkUsageCLI(t, svc, "usage", "prices", "--accept", "abc"); err == nil || !strings.Contains(err.Error(), "no pending price change") {
		t.Errorf("accept without pending: %v", err)
	}
}

func TestForkUsagePricesAutoSwitch(t *testing.T) {
	svc, _ := newForkPriceService(t)
	out, err := runForkUsageCLI(t, svc, "usage", "prices", "auto", "off")
	if err != nil || strings.TrimSpace(out) != "automatic DeepSeek price check: off" {
		t.Fatalf("auto off: %v %q", err, out)
	}
	if svc.AutoCheck() {
		t.Fatal("switch not saved")
	}
	out, err = runForkUsageCLI(t, svc, "usage", "prices")
	if err != nil || !strings.Contains(out, "check: auto off") || !strings.Contains(out, "warning: automatic price check is off") {
		t.Errorf("status: %v\n%s", err, out)
	}
	if _, err := runForkUsageCLI(t, svc, "usage", "prices", "auto", "maybe"); ExitCode(err) != 2 {
		t.Errorf("bad arg: %v", err)
	}
	if _, err := runForkUsageCLI(t, svc, "usage", "prices", "auto", "on"); err != nil || !svc.AutoCheck() {
		t.Errorf("auto on: %v", err)
	}
}

func TestForkUsagePricesOldDaemonAndRemote(t *testing.T) {
	if _, err := runForkUsageCLI(t, nil, "usage", "window"); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Errorf("old daemon: %v", err)
	}
	svc, _ := newForkPriceService(t)
	for _, args := range [][]string{
		{"usage", "prices", "--check", "--host", "studio"},
		{"usage", "prices", "auto", "off", "--host", "studio"},
	} {
		if _, err := runForkUsageCLI(t, svc, args...); err == nil || !strings.Contains(err.Error(), "cannot run against a remote host") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// A successful --accept exits 0 (exit codes 3/4 are for --check only).
func TestForkUsagePricesAcceptExitsZero(t *testing.T) {
	svc, page := newForkPriceService(t)
	fixture, err := os.ReadFile("../forkusage/pricebook/testdata/pricing-2026-11-24.md")
	if err != nil {
		t.Fatal(err)
	}
	// Pro peak cache-hit x100: suspicious, held for --accept.
	page.set(strings.Replace(string(fixture), "| $0.044 ", "| $4.4   ", 1))
	out, err := runForkUsageCLI(t, svc, "usage", "prices", "--check", "--json")
	if ExitCode(err) != exitPricesChanged {
		t.Fatalf("check: exit %d %v\n%s", ExitCode(err), err, out)
	}
	var res forkCheckResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Outcome != "suspicious" || res.PageSHA256 == "" || res.Recorded != "" {
		t.Fatalf("check json: %v\n%s", err, out)
	}
	out, err = runForkUsageCLI(t, svc, "usage", "prices", "--accept", res.PageSHA256)
	if err != nil || ExitCode(err) != 0 {
		t.Fatalf("accept: exit %d %v\n%s", ExitCode(err), err, out)
	}
	if !strings.Contains(out, "recorded: deepseek-auto-") || svc.Book().Latest().Version == "deepseek-2026-09-10" {
		t.Errorf("accept output / book: %s", out)
	}
}
