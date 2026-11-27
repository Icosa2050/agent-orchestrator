package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/providers"
)

const forkSentinelKey = "sk-SENTINEL-cli-provider-usage-42"

type forkCLIKeys struct{}

func (forkCLIKeys) Key(context.Context, string) (string, error) { return forkSentinelKey, nil }

// newForkProvidersDaemon serves the upstream accounts route plus the real
// price-book and providers routes; the providers talk to a local TLS fake.
// mode "401" makes the provider reject the key and echo it in the body.
func newForkProvidersDaemon(t *testing.T, mode string, withRoutes bool) *chi.Mux {
	t.Helper()
	fake := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "401" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"`+forkSentinelKey+`"}`)
			return
		}
		if r.URL.Path == "/user/balance" {
			_, _ = io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"42.10","granted_balance":"0.00","topped_up_balance":"42.10"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"limit":100,"limit_reset":"weekly","limit_remaining":73.2,"usage":12.4,"usage_daily":0.62,"usage_weekly":3.1,"usage_monthly":12.4}}`)
	}))
	t.Cleanup(fake.Close)
	r := chi.NewRouter()
	r.Post("/internal/telemetry/cli-invoked", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Get("/api/v1/agents/accounts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"accounts":[{"id":"device-claude","provider":"claude","email":"me@example.com","plan":"max","signedIn":true}]}`)
	})
	if withRoutes {
		book, _ := newForkPriceService(t)
		svc, err := providers.NewService(providers.Config{
			DataDir: t.TempDir(), Client: fake.Client(), Keys: forkCLIKeys{}, Window: book.WindowReport,
			DeepSeekURL: fake.URL + "/user/balance", OpenRouterURL: fake.URL + "/api/v1/key",
		})
		if err != nil {
			t.Fatal(err)
		}
		pricebook.Routes(book)(r)
		providers.Routes(svc)(r)
	}
	return r
}

func runForkProvidersCLI(t *testing.T, r http.Handler, args ...string) (string, string, error) {
	t.Helper()
	cfg := setConfigEnv(t)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	return executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, args...)
}

func TestForkUsageProvidersEnableAndLimitsRows(t *testing.T) {
	daemon := newForkProvidersDaemon(t, "ok", true)
	// Nothing enabled: `limits` prints exactly the upstream table.
	out, _, err := runForkProvidersCLI(t, daemon, "usage", "limits")
	if err != nil || strings.Contains(out, "PROVIDER") || !strings.Contains(out, "claude me@example.com") {
		t.Fatalf("limits with providers off: %v\n%s", err, out)
	}
	var all strings.Builder
	for _, p := range []string{"deepseek", "openrouter"} {
		out, errOut, err := runForkProvidersCLI(t, daemon, "usage", "providers", "enable", p)
		if err != nil || !strings.Contains(out, p+" polling enabled") {
			t.Fatalf("enable %s: %v\n%s", p, err, out)
		}
		all.WriteString(out + errOut)
	}
	out, errOut, err := runForkProvidersCLI(t, daemon, "usage", "limits")
	if err != nil {
		t.Fatal(err)
	}
	all.WriteString(out + errOut)
	t.Logf("limits:\n%s", out)
	for _, want := range []string{
		"PROVIDER", "KEY", "deepseek", "USD 42.10 (granted 0.00, topped up 42.10)", "24h change: insufficient history",
		"openrouter", "$73.20 left of $100.00 weekly", "today $0.62, week $3.10, month $12.40", "weekly, resets Mon ",
		"hint: DeepSeek is ", "prices: deepseek-", "deepseek outside AO (24h): insufficient history",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("limits missing %q:\n%s", want, out)
		}
	}
	jsonOut, _, err := runForkProvidersCLI(t, daemon, "usage", "limits", "--json")
	if err != nil {
		t.Fatal(err)
	}
	all.WriteString(jsonOut)
	var res struct {
		Accounts        []map[string]any `json:"accounts"`
		ProvidersSchema int              `json:"providersSchema"`
		Providers       []providers.Row  `json:"providers"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &res); err != nil || len(res.Accounts) != 1 || res.ProvidersSchema != 1 || len(res.Providers) != 2 {
		t.Fatalf("limits --json: %v\n%s", err, jsonOut)
	}
	ds := res.Providers[0]
	if ds.Provider != "deepseek" || ds.Status != "ok" || ds.Balance.Currency != "USD" || ds.Window == nil || ds.Window.Current == "" || ds.PriceBook == nil {
		t.Fatalf("deepseek row %+v", ds)
	}
	if strings.Contains(all.String(), "SENTINEL") {
		t.Fatalf("key leaked into CLI output:\n%s", all.String())
	}
	// disable keeps working and the row turns disabled.
	out, _, err = runForkProvidersCLI(t, daemon, "usage", "providers", "disable", "openrouter")
	if err != nil || !strings.Contains(out, "openrouter polling disabled") || !strings.Contains(out, "disabled") {
		t.Fatalf("disable: %v\n%s", err, out)
	}
}

func TestForkUsageProvidersRejectedKeyNeverLeaks(t *testing.T) {
	daemon := newForkProvidersDaemon(t, "401", true)
	out, errOut, err := runForkProvidersCLI(t, daemon, "usage", "providers", "enable", "deepseek")
	if err != nil {
		t.Fatal(err)
	}
	limits, errOut2, err := runForkProvidersCLI(t, daemon, "usage", "limits", "--json")
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ := runForkProvidersCLI(t, daemon, "usage", "providers", "status")
	all := out + errOut + limits + errOut2 + status
	if !strings.Contains(all, "invalid key (401)") || strings.Contains(all, "SENTINEL") {
		t.Fatalf("output:\n%s", all)
	}
}

func TestForkUsageProvidersOldDaemonAndRemote(t *testing.T) {
	old := newForkProvidersDaemon(t, "ok", false)
	out, _, err := runForkProvidersCLI(t, old, "usage", "limits")
	if err != nil || strings.Contains(out, "providers") || !strings.Contains(out, "claude me@example.com") {
		t.Fatalf("old daemon limits: %v\n%s", err, out)
	}
	out, _, err = runForkProvidersCLI(t, old, "usage", "limits", "--json")
	if err != nil || !strings.Contains(out, `"providersStatus": "unsupported"`) || !strings.Contains(out, `"accounts"`) {
		t.Fatalf("old daemon limits --json: %v\n%s", err, out)
	}
	if _, _, err := runForkProvidersCLI(t, old, "usage", "providers", "status"); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Fatalf("old daemon status: %v", err)
	}
	daemon := newForkProvidersDaemon(t, "ok", true)
	for _, args := range [][]string{
		{"usage", "providers", "enable", "deepseek", "--host", "studio"},
		{"usage", "providers", "disable", "openrouter", "--host", "studio"},
	} {
		if _, _, err := runForkProvidersCLI(t, daemon, args...); err == nil || !strings.Contains(err.Error(), "cannot run against a remote host") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, _, err := runForkProvidersCLI(t, daemon, "usage", "providers", "enable", "pal"); ExitCode(err) != 2 {
		t.Errorf("pal: exit %d %v", ExitCode(err), err)
	}
}

func forkFleetHost(t *testing.T, name string, rows ...providers.Row) fleetUsageHost {
	t.Helper()
	data, err := json.Marshal(providers.StatusResponse{ProvidersSchema: 1, Providers: rows})
	if err != nil {
		t.Fatal(err)
	}
	return fleetUsageHost{remoteHealth: remoteHealth{Name: name, Status: remoteStatusOK}, Data: data}
}

// The same key on several hosts is one row ("same key on: …"), newest
// reading first; different keys and keyless rows stay separate.
func TestForkUsageProvidersFleetDedupeByKeyID(t *testing.T) {
	early := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	late := early.Add(10 * time.Minute)
	bal := func(total string) *providers.Balance {
		return &providers.Balance{Currency: "USD", Total: total, Granted: "0.00", ToppedUp: total}
	}
	hosts := []fleetUsageHost{
		forkFleetHost(t, "local",
			providers.Row{Provider: "deepseek", Status: "ok", KeyID: "aaaaaaaaaaaa", FetchedAt: &early, Balance: bal("40.00")},
			providers.Row{Provider: "openrouter", Status: "disabled"}),
		forkFleetHost(t, "studio",
			providers.Row{Provider: "deepseek", Status: "ok", KeyID: "aaaaaaaaaaaa", FetchedAt: &late, Balance: bal("39.50")},
			providers.Row{Provider: "openrouter", Status: "ok", KeyID: "bbbbbbbbbbbb", FetchedAt: &late}),
		forkFleetHost(t, "mini",
			providers.Row{Provider: "deepseek", Status: "unavailable", Reason: "no key"},
			providers.Row{Provider: "openrouter", Status: "disabled"}),
		{remoteHealth: remoteHealth{Name: "gone", Status: remoteStatusOffline}},
	}
	rows := forkDedupeProviderRows(hosts)
	if len(rows) != 3 {
		t.Fatalf("rows %+v", rows)
	}
	if rows[0].KeyID != "aaaaaaaaaaaa" || rows[0].Balance.Total != "39.50" || strings.Join(rows[0].Hosts, ",") != "local,studio" {
		t.Errorf("shared key row %+v", rows[0])
	}
	if rows[1].Provider != "deepseek" || rows[1].Reason != "no key" || strings.Join(rows[1].Hosts, ",") != "mini" {
		t.Errorf("keyless row %+v", rows[1])
	}
	if rows[2].Provider != "openrouter" || rows[2].Status != "ok" {
		t.Errorf("openrouter row %+v (disabled rows of a polled provider must drop)", rows[2])
	}
	var b strings.Builder
	if err := writeForkProviderRows(&b, rows, late); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "aaaaaaaaaaaa (same key on: local, studio)") {
		t.Fatalf("text:\n%s", b.String())
	}
}

func TestForkUsageProvidersWindowHint(t *testing.T) {
	until := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	next := &providers.Span{From: until, Until: time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)}
	got := forkWindowHint(&providers.Window{Current: "peak", PeakVsOffPeak: "2", Until: &until, NextOffPeak: next})
	want := "DeepSeek is PEAK now; off-peak (half price) from Tue 10:00 UTC (11:00 Berlin) until Wed 01:00 UTC (02:00 Berlin)."
	if got != want {
		t.Errorf("peak hint\n got %q\nwant %q", got, want)
	}
	summer := time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC)
	if got := forkWindowHint(&providers.Window{Current: "off-peak", PeakVsOffPeak: "2", Until: &summer}); got != "DeepSeek is OFF-PEAK now (half price) until Wed 01:00 UTC (03:00 Berlin)." {
		t.Errorf("off-peak hint %q", got)
	}
}

// A key limit and an effective budget with different periods are each shown
// with their own period, never one budget's amounts with the other's reset.
func TestForkUsageProvidersOpenRouterBudgetAndKeyLimit(t *testing.T) {
	num := func(s string) *json.Number { n := json.Number(s); return &n }
	str := func(s string) *string { return &s }
	resets := time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)
	r := providers.Row{Provider: "openrouter", Status: "ok", Key: &providers.KeyInfo{
		LimitUSD: num("100"), RemainingUSD: num("80"), LimitReset: str("monthly"),
		Budget:   &providers.Budget{LimitUSD: num("50"), RemainingUSD: num("12.5"), ResetInterval: str("weekly"), ResetsAt: &resets},
		ResetsAt: &resets, ResetInterval: "weekly",
	}}
	balance, _, reset := forkProviderCells(r)
	if balance != "$12.50 left of $50.00 weekly budget; key $80.00 left of $100.00 monthly" || !strings.HasPrefix(reset, "weekly, resets Mon 30 Nov") {
		t.Fatalf("balance %q reset %q", balance, reset)
	}
}

func TestForkUsageProvidersCoverageAndMissingHosts(t *testing.T) {
	amount := "-1.84"
	from := time.Date(2026, 11, 24, 12, 45, 0, 0, time.UTC)
	r := providers.Row{Provider: "deepseek", Status: "ok", Change: &providers.Change{
		Window: "24h", Currency: "USD", Amount: &amount,
		Coverage:    &providers.Coverage{From: from, Partial: true, Gaps: 2},
		Adjustments: []providers.Adjustment{{Kind: "top-up"}},
	}}
	if _, used, _ := forkProviderCells(r); used != "24h change -1.84 USD (observed, since Tue 12:45 UTC only, 2 gaps, includes top-up)" {
		t.Fatalf("used %q", used)
	}
	accounts := []fleetUsageHost{
		{remoteHealth: remoteHealth{Name: "local", Status: remoteStatusOK}},
		{remoteHealth: remoteHealth{Name: "old", Status: remoteStatusOK}},
		{remoteHealth: remoteHealth{Name: "gone", Status: remoteStatusOffline}},
	}
	provs := []fleetUsageHost{
		forkFleetHost(t, "local"),
		{remoteHealth: remoteHealth{Name: "old", Status: remoteStatusError, Error: "daemon returned HTTP 404"}},
		{remoteHealth: remoteHealth{Name: "gone", Status: remoteStatusOffline}},
	}
	got := forkProviderProblems(accounts, provs)
	if len(got) != 1 || got[0].Name != "old" || got[0].Status != providers.StatusUnsupported {
		t.Fatalf("problems %+v", got)
	}
}
