package pricebook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type pageServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	body     string
	status   int
	etag     string
	requests atomic.Int32
	bad      atomic.Value // string: a header that must not have been sent
}

func newPageServer(t *testing.T, body string) *pageServer {
	t.Helper()
	ps := &pageServer{body: body, status: http.StatusOK, etag: `"v1"`}
	ps.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.requests.Add(1)
		for _, h := range []string{"Authorization", "Cookie"} {
			if r.Header.Get(h) != "" {
				ps.bad.Store(h)
			}
		}
		if r.Header.Get("User-Agent") != userAgent {
			ps.bad.Store("User-Agent " + r.Header.Get("User-Agent"))
		}
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if ps.status == http.StatusFound {
			http.SetCookie(w, &http.Cookie{Name: "track", Value: "1"})
			http.Redirect(w, r, "https://evil.example/pricing.md", http.StatusFound)
			return
		}
		if ps.status != http.StatusOK {
			w.WriteHeader(ps.status)
			return
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == ps.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", ps.etag)
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte(ps.body))
	}))
	t.Cleanup(ps.srv.Close)
	return ps
}

func (ps *pageServer) set(body, etag string, status int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.body, ps.etag, ps.status = body, etag, status
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestService(t *testing.T, ps *pageServer, c *clock) *Service {
	t.Helper()
	s, err := NewService(Config{DataDir: t.TempDir(), Client: ps.srv.Client(), URL: ps.srv.URL + "/quick_start/pricing.md", Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func realPage(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("testdata/pricing-2026-11-24.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func changedPage(t *testing.T) string {
	t.Helper()
	pro := proCol()
	pro.peak[2], pro.offPeak[2] = "$3.5", "$1.75"
	return mkPage([]pageModel{flashCol(), pro}, realAliasNote, realPeakNote)
}

func TestCheckUnchangedThen304(t *testing.T) {
	ps := newPageServer(t, realPage(t))
	c := &clock{now: ts(t, "2026-11-25T08:00:00Z")}
	s := newTestService(t, ps, c)
	res, err := s.Check(context.Background())
	if err != nil || res.Outcome != OutcomeUnchanged || res.Recorded != "" {
		t.Fatalf("first check: %+v %v", res, err)
	}
	c.Add(25 * time.Hour)
	res, err = s.Check(context.Background())
	if err != nil || res.Outcome != OutcomeUnchanged || res.Status.LastSuccessAt == nil || !res.Status.LastSuccessAt.Equal(c.Now()) {
		t.Fatalf("304 check: %+v %v", res, err)
	}
	if bad := ps.bad.Load(); bad != nil {
		t.Fatalf("request carried %v", bad)
	}
	if _, err := os.Stat(AutoVersionsPath(s.cfg.DataDir)); !os.IsNotExist(err) {
		t.Errorf("unchanged page wrote auto versions: %v", err)
	}
}

func TestCheckRecordsChangeKeepingOldVersion(t *testing.T) {
	ps := newPageServer(t, changedPage(t))
	c := &clock{now: ts(t, "2026-12-03T06:12:00Z")}
	s := newTestService(t, ps, c)
	res, err := s.Check(context.Background())
	if err != nil || res.Outcome != OutcomeChanged || res.Recorded != "deepseek-auto-2026-12-03" || res.Diff == nil {
		t.Fatalf("check: %+v %v", res, err)
	}
	if res.Status.Version != "deepseek-auto-2026-12-03" || res.Status.Label != AutoLabel || res.Status.Source != SourcePageAuto {
		t.Errorf("status: %+v", res.Status)
	}
	info, err := os.Stat(AutoVersionsPath(s.cfg.DataDir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auto file: %v %v", info, err)
	}
	book := s.Book()
	before, _ := book.Price(Usage{Route: "deepseek", Model: "deepseek-v4-pro", At: ts(t, "2026-12-03T06:11:59Z"), OutputTokens: 1_000_000})
	after, _ := book.Price(Usage{Route: "deepseek", Model: "deepseek-v4-pro", At: ts(t, "2026-12-03T06:12:00Z"), OutputTokens: 1_000_000})
	if before.TotalNanos != 3_960_000_000 || after.TotalNanos != 3_500_000_000 {
		t.Errorf("prices around the detection instant: before %+v after %+v", before, after)
	}
	// A restarted service reads the recorded version back.
	s2, err := NewService(Config{DataDir: s.cfg.DataDir, URL: SourcePage, Now: c.Now})
	if err != nil || s2.Book().Latest().Version != "deepseek-auto-2026-12-03" || len(s2.Book().Versions()) != 3 {
		t.Fatalf("reload: %v %v", s2.Book().Versions(), err)
	}
	// The same page again is unchanged against the new version.
	c.Add(time.Hour)
	ps.set(changedPage(t), `"v2"`, http.StatusOK)
	if res, _ := s.Check(context.Background()); res.Outcome != OutcomeUnchanged {
		t.Errorf("recheck: %+v", res)
	}
}

func TestCheckSuspiciousNeedsAccept(t *testing.T) {
	pro := proCol()
	pro.peak[0], pro.offPeak[0] = "$0.5", "$0.25"
	ps := newPageServer(t, mkPage([]pageModel{flashCol(), pro}, realAliasNote, realPeakNote))
	c := &clock{now: ts(t, "2026-12-03T06:12:00Z")}
	s := newTestService(t, ps, c)
	res, err := s.Check(context.Background())
	if err != nil || res.Outcome != OutcomeSuspicious || res.Recorded != "" || res.Status.Pending == nil {
		t.Fatalf("check: %+v %v", res, err)
	}
	if s.Book().Latest().Version != "deepseek-2026-09-10" {
		t.Fatal("suspicious change was recorded")
	}
	if !strings.Contains(strings.Join(res.Status.Warnings, "\n"), "--accept "+res.PageSHA256) {
		t.Errorf("warnings: %v", res.Status.Warnings)
	}
	if _, err := s.Accept("0000"); err == nil {
		t.Error("accept with a wrong hash succeeded")
	}
	c.Add(2 * time.Hour)
	acc, err := s.Accept(res.PageSHA256)
	if err != nil || acc.Recorded != "deepseek-auto-2026-12-03" || acc.Status.Pending != nil {
		t.Fatalf("accept: %+v %v", acc, err)
	}
	// Effective from the fetch that found it, not the accept.
	if v, _ := s.Book().VersionAt(ts(t, "2026-12-03T06:12:00Z")); v.Version != "deepseek-auto-2026-12-03" {
		t.Errorf("accepted version effective at fetch: %+v", v)
	}
}

func TestCheckFailuresKeepLastVersion(t *testing.T) {
	ps := newPageServer(t, "<html><body>maintenance</body></html>")
	c := &clock{now: ts(t, "2026-11-25T08:00:00Z")}
	s := newTestService(t, ps, c)
	for _, tc := range []struct {
		body   string
		status int
		reason string
	}{
		{"<html><body>maintenance</body></html>", http.StatusOK, "HTML instead of Markdown"},
		{realPage(t)[:2000], http.StatusOK, "unrecognized page layout"},
		{"", http.StatusInternalServerError, "http status 500"},
		{"", http.StatusFound, "redirects are refused"},
		{strings.Repeat("x", maxPageBytes+10), http.StatusOK, "larger than 512 KB"},
	} {
		ps.set(tc.body, `"e"`, tc.status)
		res, err := s.Check(context.Background())
		if err != nil || res.Outcome != OutcomeCannotVerify || !strings.Contains(res.Reason, tc.reason) {
			t.Errorf("%q: %+v %v", tc.reason, res, err)
		}
		if !strings.Contains(strings.Join(res.Status.Warnings, "\n"), "price check failed") {
			t.Errorf("%q: no warning: %v", tc.reason, res.Status.Warnings)
		}
	}
	if bad := ps.bad.Load(); bad != nil {
		t.Errorf("request carried %v (a redirect cookie must not be replayed)", bad)
	}
	if s.Book().Latest().Version != "deepseek-2026-09-10" {
		t.Error("a failed check changed the price book")
	}
	// Not stale yet: the embedded version was verified on 2026-11-24.
	if s.Status().Stale {
		t.Error("stale one day after the last verification")
	}
	c.Add(8 * 24 * time.Hour)
	if !s.Status().Stale {
		t.Error("not stale eight days after the last good check")
	}
}

func TestCheckNetworkErrorAndHTTPSOnly(t *testing.T) {
	if _, err := NewService(Config{DataDir: t.TempDir(), URL: "http://api-docs.deepseek.com/quick_start/pricing.md"}); err == nil {
		t.Error("plain http URL accepted")
	}
	ps := newPageServer(t, realPage(t))
	c := &clock{now: ts(t, "2026-11-25T08:00:00Z")}
	s := newTestService(t, ps, c)
	ps.srv.Close()
	res, err := s.Check(context.Background())
	if err != nil || res.Outcome != OutcomeCannotVerify || res.Reason != "network error" {
		t.Errorf("closed server: %+v %v", res, err)
	}
}

func TestRunChecksAtMostDailyAndHonoursSwitch(t *testing.T) {
	ps := newPageServer(t, realPage(t))
	c := &clock{now: ts(t, "2026-11-25T08:00:00Z")}
	ticks := make(chan time.Time)
	waited := make(chan time.Duration, 100)
	s, err := NewService(Config{
		DataDir: t.TempDir(), Client: ps.srv.Client(), URL: ps.srv.URL + "/p.md", Now: c.Now,
		FirstDelay: func() time.Duration { return 7 * time.Minute },
		After: func(d time.Duration) <-chan time.Time {
			waited <- d
			return ticks
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	tick := func() {
		<-waited
		ticks <- c.Now()
	}
	tick() // first check after the jittered delay
	tick() // one hour later: not due
	for i := 0; i < 5; i++ {
		c.Add(time.Hour)
		tick()
	}
	<-waited
	if got := ps.requests.Load(); got != 1 {
		t.Fatalf("requests within 24 h = %d, want 1", got)
	}
	c.Add(24 * time.Hour)
	ticks <- c.Now()
	<-waited
	if got := ps.requests.Load(); got != 2 {
		t.Fatalf("requests after 24 h = %d, want 2", got)
	}
	// Switched off: no request however long it waits.
	if err := s.SetAutoCheck(false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c.Add(48 * time.Hour)
		ticks <- c.Now()
		<-waited
	}
	if got := ps.requests.Load(); got != 2 {
		t.Fatalf("requests with the switch off = %d, want 2", got)
	}
	cancel()
	<-done
}

func TestAutoCheckSwitchPreservesOtherConfig(t *testing.T) {
	dir := t.TempDir()
	path := ConfigPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"providers":{"deepseek":{"enabled":true}},"pricebook":{"autoCheck":true,"x":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !readAutoCheck(dir) {
		t.Fatal("default on")
	}
	if err := writeAutoCheck(dir, false); err != nil {
		t.Fatal(err)
	}
	if readAutoCheck(dir) {
		t.Fatal("still on")
	}
	var cfg map[string]map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &cfg); err != nil || cfg["providers"] == nil || cfg["pricebook"]["x"] == nil {
		t.Errorf("config lost keys: %s", data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v", info.Mode())
	}
	if !readAutoCheck(t.TempDir()) {
		t.Error("missing config is not default on")
	}
}

func TestCorruptAutoFileFallsBackToEmbedded(t *testing.T) {
	dir := t.TempDir()
	path := AutoVersionsPath(dir)
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	s, err := NewService(Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.Version != "deepseek-2026-09-10" || !strings.Contains(strings.Join(st.Warnings, "\n"), "could not be read") {
		t.Errorf("status: %+v", st)
	}
}

func TestAcceptRefusesNonUSD(t *testing.T) {
	yuan := strings.ReplaceAll(mkPage([]pageModel{flashCol(), proCol()}, realAliasNote, realPeakNote), "$", "¥")
	ps := newPageServer(t, yuan)
	s := newTestService(t, ps, &clock{now: ts(t, "2026-12-03T06:12:00Z")})
	res, err := s.Check(context.Background())
	if err != nil || res.Outcome != OutcomeSuspicious || res.Status.Pending == nil {
		t.Fatalf("check: %+v %v", res, err)
	}
	if _, err := s.Accept(res.PageSHA256); err == nil || !strings.Contains(err.Error(), "USD only") {
		t.Fatalf("accept non-USD: %v", err)
	}
	if s.Book().Latest().Version != "deepseek-2026-09-10" {
		t.Error("non-USD prices activated")
	}
}
