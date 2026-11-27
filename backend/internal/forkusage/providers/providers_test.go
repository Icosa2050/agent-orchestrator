package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
)

const sentinelKey = "sk-SENTINEL-provider-usage-0123456789"

type staticKeys map[string]string

func (k staticKeys) Key(_ context.Context, p string) (string, error) {
	if v, ok := k[p]; ok {
		return v, nil
	}
	return "", errNoKey
}

// fakeProviders serves both provider endpoints over TLS and records the
// Authorization header of each request.
type fakeProviders struct {
	mu    sync.Mutex
	mode  string
	auths []string
	paths []string
	srv   *httptest.Server
}

func newFakeProviders(t *testing.T) *fakeProviders {
	t.Helper()
	f := &fakeProviders{mode: "ok"}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		mode := f.mode
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		switch mode {
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"bad key `+sentinelKey+`"}`)
			return
		case "timeout":
			time.Sleep(300 * time.Millisecond)
		case "redirect":
			http.Redirect(w, r, "https://evil.example/"+sentinelKey, http.StatusFound)
			return
		case "malformed":
			_, _ = io.WriteString(w, `{"is_available": true, "echo": "`+sentinelKey+`"`)
			return
		case "big":
			_, _ = io.WriteString(w, strings.Repeat(" ", maxBodyBytes+10))
			return
		}
		if r.URL.Path == "/user/balance" {
			_, _ = io.WriteString(w, `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"42.10","granted_balance":"0.00","topped_up_balance":"42.10"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"label":"sk-or-v1-abc...xyz","limit":100,"limit_reset":"weekly","limit_remaining":73.2,"usage":12.4,`+
			`"usage_daily":0.62,"usage_weekly":3.1,"usage_monthly":12.4,"is_free_tier":false}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProviders) set(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode = mode
}

func (f *fakeProviders) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.auths)
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestService(t *testing.T, f *fakeProviders, keys KeyReader, clock *testClock, log io.Writer) *Service {
	t.Helper()
	if log == nil {
		log = io.Discard
	}
	svc, err := NewService(Config{
		DataDir: t.TempDir(), Client: f.srv.Client(), Keys: keys, Now: clock.now,
		Logger:      slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DeepSeekURL: f.srv.URL + "/user/balance", OpenRouterURL: f.srv.URL + "/api/v1/key",
		Timeout: 100 * time.Millisecond, Jitter: func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func rowOf(t *testing.T, res StatusResponse, provider string) Row {
	t.Helper()
	for _, r := range res.Providers {
		if r.Provider == provider {
			return r
		}
	}
	t.Fatalf("no %s row in %+v", provider, res)
	return Row{}
}

func TestProvidersOffByDefaultSendNothing(t *testing.T) {
	f := newFakeProviders(t)
	clock := &testClock{t: time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)}
	svc := newTestService(t, f, staticKeys{DeepSeek: sentinelKey, OpenRouter: sentinelKey}, clock, nil)
	ctx, cancel := context.WithCancel(context.Background())
	svc.cfg.Tick = time.Millisecond
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	res := svc.Status(context.Background(), true)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if n := f.requests(); n != 0 {
		t.Fatalf("%d requests with every provider off", n)
	}
	for _, r := range res.Providers {
		if r.Status != StatusDisabled || r.KeyID != "" {
			t.Errorf("row %+v", r)
		}
	}
	if res.ProvidersSchema != 1 || res.Thresholds != DefaultThresholds {
		t.Errorf("schema/thresholds %+v", res)
	}
}

func TestProvidersDeepSeekBalanceAndOpenRouterKey(t *testing.T) {
	f := newFakeProviders(t)
	clock := &testClock{t: time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)} // Tuesday
	svc := newTestService(t, f, staticKeys{DeepSeek: sentinelKey, OpenRouter: sentinelKey + "-or"}, clock, nil)
	for _, p := range Known {
		if err := svc.SetEnabled(p, true); err != nil {
			t.Fatal(err)
		}
	}
	res := svc.Status(context.Background(), true)
	ds := rowOf(t, res, DeepSeek)
	if ds.Status != StatusOK || ds.KeyID != KeyID(sentinelKey) || len(ds.KeyID) != 12 || ds.Balance == nil ||
		*ds.Balance != (Balance{Currency: "USD", Total: "42.10", Granted: "0.00", ToppedUp: "42.10"}) || ds.IsAvailable == nil || !*ds.IsAvailable {
		t.Fatalf("deepseek row %+v", ds)
	}
	if ds.Change == nil || ds.Change.Amount != nil || ds.Change.Reason != "insufficient history" || ds.OutsideAO == nil || ds.OutsideAO.Amount != nil {
		t.Fatalf("first sample change %+v / %+v", ds.Change, ds.OutsideAO)
	}
	or := rowOf(t, res, OpenRouter)
	if or.Status != StatusOK || or.Key == nil || string(*or.Key.RemainingUSD) != "73.2" || *or.Key.RemainingPct != 73.2 ||
		or.Key.ResetsAt == nil || !or.Key.ResetsAt.Equal(time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)) || or.Key.ResetInterval != "weekly" {
		t.Fatalf("openrouter row %+v", or.Key)
	}
	for _, a := range f.auths {
		if a != "Bearer "+sentinelKey && a != "Bearer "+sentinelKey+"-or" {
			t.Errorf("auth header %q", a)
		}
	}
	// Fresh readings are served from memory; OpenRouter's cache is 5 min.
	n := f.requests()
	svc.Status(context.Background(), true)
	if f.requests() != n {
		t.Fatalf("fresh readings refetched")
	}
	clock.add(6 * time.Minute)
	svc.Status(context.Background(), true)
	if f.requests() != n+2 {
		t.Fatalf("stale readings not refreshed: %d -> %d", n, f.requests())
	}
	// Two DeepSeek samples now: the change is observed.
	ds = rowOf(t, svc.Status(context.Background(), false), DeepSeek)
	if ds.Change.Amount == nil || *ds.Change.Amount != "0.00" || ds.Change.Coverage.Samples != 2 {
		t.Fatalf("change %+v", ds.Change)
	}
	// Disabling drops the reading and sends nothing more.
	if err := svc.SetEnabled(OpenRouter, false); err != nil {
		t.Fatal(err)
	}
	n = f.requests()
	if r := rowOf(t, svc.Status(context.Background(), true), OpenRouter); r.Status != StatusDisabled || r.Key != nil || r.KeyID != "" {
		t.Fatalf("disabled row %+v", r)
	}
	for _, p := range f.paths[n:] {
		if p == "/api/v1/key" {
			t.Fatal("disabled OpenRouter was fetched")
		}
	}
	info, err := os.Stat(BalanceHistoryPath(svc.cfg.DataDir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("history file %v %v", info, err)
	}
}

func TestProvidersErrorBackoffAndFixedReasons(t *testing.T) {
	f := newFakeProviders(t)
	clock := &testClock{t: time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)}
	svc := newTestService(t, f, staticKeys{DeepSeek: sentinelKey}, clock, nil)
	_ = svc.SetEnabled(DeepSeek, true)
	f.set("401")
	if r := rowOf(t, svc.Status(context.Background(), true), DeepSeek); r.Status != StatusUnavailable || r.Reason != ReasonInvalidKey {
		t.Fatalf("401 row %+v", r)
	}
	n := f.requests()
	clock.add(10 * time.Minute) // past FreshFor, inside the 15 min backoff
	svc.Status(context.Background(), true)
	if f.requests() != n {
		t.Fatal("refetched while backing off")
	}
	// Re-enabling clears the backoff: a fixed key is tried at once.
	f.set("ok")
	_ = svc.SetEnabled(DeepSeek, true)
	if r := rowOf(t, svc.Status(context.Background(), true), DeepSeek); r.Status != StatusOK {
		t.Fatalf("after re-enable %+v", r)
	}
	// No key at all.
	_ = svc.SetEnabled(OpenRouter, true)
	if r := rowOf(t, svc.Status(context.Background(), true), OpenRouter); r.Status != StatusUnavailable || r.Reason != ReasonNoKey {
		t.Fatalf("no key row %+v", r)
	}
}

// A sentinel key never reaches the status JSON, the log or any file under
// the data dir, whatever the provider answers.
func TestProvidersSentinelKeyNeverLeaks(t *testing.T) {
	for _, mode := range []string{"ok", "401", "timeout", "redirect", "malformed", "big"} {
		t.Run(mode, func(t *testing.T) {
			f := newFakeProviders(t)
			f.set(mode)
			clock := &testClock{t: time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)}
			var log bytes.Buffer
			svc := newTestService(t, f, staticKeys{DeepSeek: sentinelKey, OpenRouter: sentinelKey}, clock, &log)
			r := chi.NewRouter()
			Routes(svc)(r)
			var out bytes.Buffer
			for _, p := range Known {
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RoutePath, strings.NewReader(`{"provider":"`+p+`","enabled":true}`)))
				out.Write(rec.Body.Bytes())
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RoutePath, nil))
			out.Write(rec.Body.Bytes())
			if mode != "ok" && !strings.Contains(out.String(), `"unavailable"`) {
				t.Errorf("mode %s not reported unavailable: %s", mode, out.String())
			}
			if strings.Contains(out.String(), "SENTINEL") || strings.Contains(log.String(), "SENTINEL") {
				t.Fatalf("key leaked\nJSON: %s\nlog: %s", out.String(), log.String())
			}
			_ = filepath.WalkDir(svc.cfg.DataDir, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, _ := os.ReadFile(path) //nolint:gosec // test temp dir
				if bytes.Contains(data, []byte("SENTINEL")) {
					t.Errorf("key in %s", path)
				}
				return nil
			})
			if mode == "redirect" && f.requests() != 2 {
				t.Errorf("redirect followed: %d requests", f.requests())
			}
		})
	}
}

func TestProvidersRoutesRefuseLANWrites(t *testing.T) {
	f := newFakeProviders(t)
	svc := newTestService(t, f, staticKeys{}, &testClock{t: time.Now()}, nil)
	r := chi.NewRouter()
	Routes(svc)(r)
	req := httptest.NewRequest(http.MethodPost, RoutePath, strings.NewReader(`{"provider":"deepseek","enabled":true}`))
	req = req.WithContext(requestscope.WithLAN(req.Context()))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || svc.Enabled()[DeepSeek] {
		t.Fatalf("LAN enable: %d %s", rec.Code, rec.Body)
	}
	for body, want := range map[string]int{
		`{"provider":"pal","enabled":true}`:      http.StatusBadRequest,
		`{"provider":"deepseek"}`:                http.StatusBadRequest,
		`{"provider":"deepseek","enabled":true}`: http.StatusOK,
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RoutePath, strings.NewReader(body)))
		if rec.Code != want {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
}

// Enabling a provider keeps the price book's settings in the shared file.
func TestProvidersConfigKeepsPriceBookSection(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(pricebook.ConfigPath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pricebook.ConfigPath(dir), []byte(`{"pricebook":{"autoCheck":false},"thresholds":{"runwayLowDays":5}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeEnabled(dir, DeepSeek, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(pricebook.ConfigPath(dir))
	var cfg map[string]map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil || cfg["pricebook"]["autoCheck"] != false {
		t.Fatalf("config %s", data)
	}
	on, th := readConfig(dir)
	if !on[DeepSeek] || on[OpenRouter] || th.RunwayLowDays != 5 || th.KeyRemainingLowPct != 25 {
		t.Fatalf("on=%v th=%+v", on, th)
	}
	// Valid-but-null sections are rewritten, not a panic.
	for _, body := range []string{`{"providers":null}`, `{"providers":{"deepseek":null}}`, `null`} {
		if err := os.WriteFile(pricebook.ConfigPath(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := writeEnabled(dir, DeepSeek, true); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if on, _ := readConfig(dir); !on[DeepSeek] {
			t.Fatalf("%s: not enabled", body)
		}
	}
	if err := writeEnabled(dir, "pal", true); !errors.Is(err, errUnknownProvider) {
		t.Fatalf("pal: %v", err)
	}
}

func TestParseBalanceStrictDecimals(t *testing.T) {
	yes := true
	mk := func(total string) deepseekBalanceResponse {
		var r deepseekBalanceResponse
		r.IsAvailable = &yes
		r.BalanceInfos = append(r.BalanceInfos, struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		}{"CNY", total, "0", total})
		return r
	}
	for total, ok := range map[string]bool{"42.10": true, "0": true, "": false, "1e3": false, "+1": false, "abc": false, "1.": false} {
		if _, _, got := parseBalance(mk(total)); got != ok {
			t.Errorf("%q: ok=%v", total, got)
		}
	}
	if _, _, ok := parseBalance(deepseekBalanceResponse{IsAvailable: &yes}); ok {
		t.Error("empty balance_infos accepted")
	}
	both := mk("1.00")
	both.BalanceInfos = append(both.BalanceInfos, both.BalanceInfos[0])
	if _, _, ok := parseBalance(both); ok {
		t.Error("duplicate currency accepted")
	}
}

func sample(ts time.Time, key, cur, total, granted, topped string) Sample {
	return Sample{TS: ts, KeyID: key, Currency: cur, Total: total, Granted: granted, ToppedUp: topped}
}

func TestComputeChange(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	at := func(h float64) time.Time { return now.Add(time.Duration(-h * float64(time.Hour))) }
	t.Run("spend", func(t *testing.T) {
		ch := computeChange([]Sample{
			sample(at(24.1), "k1", "USD", "44.00", "0", "44.00"),
			sample(at(12), "k1", "USD", "43.00", "0", "43.00"),
			sample(at(0), "k1", "USD", "42.16", "0", "42.16"),
		}, "k1", "USD", now)
		if ch.Amount == nil || *ch.Amount != "-1.84" || ch.Coverage.Gaps != 2 || ch.Coverage.Partial || len(ch.Adjustments) != 0 {
			t.Fatalf("%+v %+v", ch, ch.Coverage)
		}
		if o := outsideAO(ch); o.Amount != nil || !strings.Contains(o.Reason, "not joined") {
			t.Fatalf("outside %+v", o)
		}
	})
	t.Run("an exact boundary sample beats an older one", func(t *testing.T) {
		ch := computeChange([]Sample{
			sample(at(24.25), "k1", "USD", "100.00", "0", "100.00"),
			sample(at(24), "k1", "USD", "90.00", "0", "90.00"),
			sample(at(0), "k1", "USD", "90.00", "0", "90.00"),
		}, "k1", "USD", now)
		if ch.Amount == nil || *ch.Amount != "0.00" || ch.Coverage.Samples != 2 {
			t.Fatalf("%+v", ch)
		}
	})
	t.Run("top-up and grant are adjustments", func(t *testing.T) {
		ch := computeChange([]Sample{
			sample(at(2), "k1", "USD", "1.00", "0.50", "0.50"),
			sample(at(1), "k1", "USD", "11.00", "0.50", "10.50"),
			sample(at(0), "k1", "USD", "12.00", "1.50", "10.50"),
		}, "k1", "USD", now)
		if len(ch.Adjustments) != 2 || ch.Adjustments[0].Kind != "top-up" || ch.Adjustments[1].Kind != "grant" || !ch.Coverage.Partial {
			t.Fatalf("%+v", ch)
		}
		if o := outsideAO(ch); o.Amount != nil || !strings.Contains(o.Reason, "top-up detected") {
			t.Fatalf("outside %+v", o)
		}
	})
	t.Run("key change and currency switch start a new baseline", func(t *testing.T) {
		samples := []Sample{
			sample(at(3), "old", "USD", "90.00", "0", "90.00"),
			sample(at(2), "k1", "CNY", "10.00", "0", "10.00"),
			sample(at(1), "k1", "USD", "5.00", "0", "5.00"),
		}
		if ch := computeChange(samples, "k1", "USD", now); ch.Amount != nil || ch.Reason != "insufficient history" {
			t.Fatalf("%+v", ch)
		}
	})
}

func TestBalanceHistoryRetentionAndDamage(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	path := BalanceHistoryPath(dir)
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	old, _ := json.Marshal(sample(now.Add(-91*24*time.Hour), "k", "USD", "1", "0", "1"))
	keep, _ := json.Marshal(sample(now.Add(-time.Hour), "k", "USD", "2", "0", "2"))
	content := string(old) + "\n" + "not json\n" + strings.Repeat("x", maxSampleLine+1) + "\n" + string(keep) + "\n" + `{"ts":"2026-11-24T11:30:00Z","keyId":"k"` // partial last line
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendSamples(dir, now, []Sample{sample(now, "k", "USD", "3", "0", "3")}); err != nil {
		t.Fatal(err)
	}
	got, err := readSamples(dir)
	if err != nil || len(got) != 2 || got[0].Total != "2" || got[1].Total != "3" {
		t.Fatalf("samples %+v %v", got, err)
	}
	// A clean file is appended to.
	if err := appendSamples(dir, now.Add(time.Minute), []Sample{sample(now.Add(time.Minute), "k", "USD", "4", "0", "4")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := readSamples(dir); len(got) != 3 {
		t.Fatalf("after append %+v", got)
	}
}

func TestParseKeyResets(t *testing.T) {
	now := time.Date(2026, 11, 29, 23, 0, 0, 0, time.UTC) // Sunday
	cases := map[string]time.Time{
		"daily":   time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC),
		"weekly":  time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC),
		"monthly": time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	}
	for interval, want := range cases {
		if got := nextReset(interval, now); got == nil || !got.Equal(want) {
			t.Errorf("%s: %v", interval, got)
		}
	}
	if got := nextReset("weekly", time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)); !got.Equal(time.Date(2026, 12, 7, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("monday weekly: %v", got)
	}
	var resp openrouterKeyResponse
	body := `{"data":{"limit":null,"limit_reset":null,"limit_remaining":null,"usage":1.5,"usage_daily":0,"usage_weekly":0,"usage_monthly":1.5,` +
		`"effective_budget":{"limit_usd":50,"spend_usd":10,"remaining_usd":40,"reset_interval":"monthly","resets_at":"2026-12-01T00:00:00Z"}}}`
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	k, ok := parseKey(resp, now)
	if !ok || k.ResetBasis != "effective_budget" || k.ResetInterval != "monthly" || k.RemainingPct != nil || string(*k.Budget.RemainingUSD) != "40" {
		t.Fatalf("%+v", k)
	}
}

func TestSystemKeysDeepSeekStoreAndEnv(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"DSH_HOME": dir}
	keys := SystemKeys{Env: func(k string) string { return env[k] }, GOOS: "linux"}
	if _, err := keys.Key(context.Background(), DeepSeek); !errors.Is(err, errNoKey) {
		t.Fatalf("empty: %v", err)
	}
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, ".credentials.yaml"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("version: 1\nrefs:\n  OPENCODE_API_KEY: other-key-1234\n  DEEPSEEK_API_KEY: sk-stored-flat-123\n")
	if k, err := keys.Key(context.Background(), DeepSeek); err != nil || k != "sk-stored-flat-123" {
		t.Fatalf("flat ref: %q %v", k, err)
	}
	write("version: 1\nrefs:\n  DEEPSEEK_API_KEY:\n    payload:\n      secret: sk-stored-nested-123\n")
	if k, _ := keys.Key(context.Background(), DeepSeek); k != "sk-stored-nested-123" {
		t.Fatalf("nested: %q", k)
	}
	write("DEEPSEEK_API_KEY: sk-prerelease-flat-1\n") // no version: Harness refuses it
	env["DEEPSEEK_API_KEY"] = "sk-from-env-12345"
	if k, _ := keys.Key(context.Background(), DeepSeek); k != "sk-from-env-12345" {
		t.Fatalf("env fallback: %q", k)
	}
	env["DEEPSEEK_API_KEY"] = "bad key\nX-Injected: 1"
	if _, err := keys.Key(context.Background(), DeepSeek); !errors.Is(err, errKeyInvalid) {
		t.Fatalf("header injection: %v", err)
	}
	// An unusable stored key does not shadow a good env key.
	write("version: 1\nrefs:\n  DEEPSEEK_API_KEY: \"garbled key\"\n")
	env["DEEPSEEK_API_KEY"] = "sk-from-env-12345"
	if k, err := keys.Key(context.Background(), DeepSeek); err != nil || k != "sk-from-env-12345" {
		t.Fatalf("stored unusable, env good: %q %v", k, err)
	}
}

func TestSystemKeysOpenRouterKeychainSecretOnStdout(t *testing.T) {
	var argv [][]string
	env := map[string]string{"USER": "owner", "OPENROUTER_API_KEY": "sk-or-env-123456"}
	keys := SystemKeys{
		Env: func(k string) string { return env[k] }, GOOS: "darwin",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			argv = append(argv, append([]string{name}, args...))
			return []byte(sentinelKey + "\n"), nil
		},
	}
	k, err := keys.Key(context.Background(), OpenRouter)
	if err != nil || k != sentinelKey {
		t.Fatalf("keychain: %q %v", k, err)
	}
	want := "security find-generic-password -s OPENROUTER_API_KEY -a owner -w"
	if len(argv) != 1 || strings.Join(argv[0], " ") != want {
		t.Fatalf("argv %v", argv)
	}
	for _, a := range argv[0] {
		if strings.Contains(a, "SENTINEL") {
			t.Fatal("key in argv")
		}
	}
	keys.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("exit 44") }
	if k, _ := keys.Key(context.Background(), OpenRouter); k != "sk-or-env-123456" {
		t.Fatalf("env fallback: %q", k)
	}
	keys.GOOS = "linux"
	keys.Run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("keychain off macOS")
		return nil, nil
	}
	if k, _ := keys.Key(context.Background(), OpenRouter); k != "sk-or-env-123456" {
		t.Fatalf("linux env: %q", k)
	}
}

// A refresh that was waiting for the fetch lock while the provider was
// disabled sends nothing.
func TestProvidersDisableWinsOverWaitingRefresh(t *testing.T) {
	f := newFakeProviders(t)
	clock := &testClock{t: time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)}
	svc := newTestService(t, f, staticKeys{DeepSeek: sentinelKey}, clock, nil)
	if err := writeEnabled(svc.cfg.DataDir, DeepSeek, true); err != nil {
		t.Fatal(err)
	}
	st := svc.state[DeepSeek]
	st.mu.Lock()
	done := make(chan struct{})
	go func() { svc.refreshIfDue(context.Background(), DeepSeek, FreshFor); close(done) }()
	time.Sleep(50 * time.Millisecond) // the refresh is now waiting for st.mu
	if err := writeEnabled(svc.cfg.DataDir, DeepSeek, false); err != nil {
		t.Fatal(err)
	}
	st.mu.Unlock()
	<-done
	if n := f.requests(); n != 0 {
		t.Fatalf("%d requests after disable", n)
	}
}

// The background loop polls an enabled DeepSeek once per PollInterval and
// never touches OpenRouter (on demand only).
func TestProvidersRunPollsEnabledDeepSeek(t *testing.T) {
	f := newFakeProviders(t)
	clock := &testClock{t: time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)}
	svc := newTestService(t, f, staticKeys{DeepSeek: sentinelKey, OpenRouter: sentinelKey}, clock, nil)
	_ = svc.SetEnabled(DeepSeek, true)
	_ = svc.SetEnabled(OpenRouter, true)
	ticks := make(chan time.Time)
	svc.cfg.After = func(time.Duration) <-chan time.Time { return ticks }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	tick := func() { ticks <- clock.now() }
	tick()
	tick() // same instant: not due again
	clock.add(PollInterval)
	tick()
	tick() // wait for the third tick's poll to finish
	cancel()
	<-done
	if n := f.requests(); n != 2 {
		t.Fatalf("%d polls, want 2", n)
	}
	for _, p := range f.paths {
		if p != "/user/balance" {
			t.Fatalf("background loop requested %s", p)
		}
	}
	if got, _ := readSamples(svc.cfg.DataDir); len(got) != 2 {
		t.Fatalf("samples %d", len(got))
	}
}

func TestParseKeyRejectsEmptyData(t *testing.T) {
	var resp openrouterKeyResponse
	if err := json.Unmarshal([]byte(`{"data":{}}`), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := parseKey(resp, time.Now()); ok {
		t.Fatal("empty data accepted")
	}
}
