package pricebook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Check limits.
const (
	CheckInterval      = 24 * time.Hour
	fetchTimeout       = 10 * time.Second
	maxPageBytes       = 512 << 10
	userAgent          = "ao-pricebook-check"
	staleAutoCheck     = 7 * 24 * time.Hour
	staleEmbedded      = 30 * 24 * time.Hour
	defaultTick        = time.Hour
	firstCheckEarliest = 5 * time.Minute
	firstCheckRange    = 25 * time.Minute
)

// Check outcomes beyond the Compare outcomes.
const (
	OutcomeCannotVerify = "cannot-verify"
)

// Paths under the data dir.
func providerUsageDir(dataDir string) string { return filepath.Join(dataDir, "provider-usage") }

// ConfigPath is the provider-usage config file (shared with later provider
// settings; unknown keys are preserved).
func ConfigPath(dataDir string) string {
	return filepath.Join(providerUsageDir(dataDir), "config.json")
}

// AutoVersionsPath holds versions the check recorded from the page.
func AutoVersionsPath(dataDir string) string {
	return filepath.Join(providerUsageDir(dataDir), "pricebook", "deepseek-auto.json")
}

func statePath(dataDir string) string {
	return filepath.Join(providerUsageDir(dataDir), "pricebook", "check-state.json")
}

type autoFile struct {
	SchemaVersion int       `json:"schemaVersion"`
	Versions      []Version `json:"versions"`
}

type pending struct {
	PageSHA256 string    `json:"pageSha256"`
	FetchedAt  time.Time `json:"fetchedAt"`
	Diff       Diff      `json:"diff"`
	Version    Version   `json:"version"`
}

type checkState struct {
	LastCheckAt     *time.Time `json:"lastCheckAt,omitempty"`
	LastCheckStatus string     `json:"lastCheckStatus,omitempty"`
	LastCheckReason string     `json:"lastCheckReason,omitempty"`
	LastSuccessAt   *time.Time `json:"lastSuccessAt,omitempty"`
	ETag            string     `json:"etag,omitempty"`
	LastModified    string     `json:"lastModified,omitempty"`
	PageSHA256      string     `json:"pageSha256,omitempty"`
	ComparedVersion string     `json:"comparedVersion,omitempty"`
	Pending         *pending   `json:"pending,omitempty"`
}

// Config wires a Service. Zero values are production defaults.
type Config struct {
	DataDir string
	// Client sends the page request; nil uses a client on
	// http.DefaultTransport. Its redirect policy, cookie jar and timeout are
	// overridden: redirects are refused and no cookies are sent.
	Client *http.Client
	// URL is SourcePage in production; it must be https.
	URL    string
	Now    func() time.Time
	Logger *slog.Logger
	// FirstDelay is the wait before the first automatic check (default a
	// random 5-30 min); After waits (default time.After).
	FirstDelay func() time.Duration
	After      func(time.Duration) <-chan time.Time
	Tick       time.Duration
}

// Service owns the merged price book and the price-page check.
type Service struct {
	cfg     Config
	client  *http.Client
	checkMu sync.Mutex // serializes checks and accepts
	mu      sync.Mutex
	book    *Book
	state   checkState
	loadErr string
}

// NewService loads the embedded book, the local auto versions and the check
// state. A corrupt local file is reported as a warning and ignored, so the
// embedded prices always work.
func NewService(cfg Config) (*Service, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("pricebook: data dir required")
	}
	if cfg.URL == "" {
		cfg.URL = SourcePage
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("pricebook: price page URL must be https")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.FirstDelay == nil {
		cfg.FirstDelay = func() time.Duration {
			return firstCheckEarliest + time.Duration(rand.Int64N(int64(firstCheckRange))) //nolint:gosec // Scheduling jitter, not security.
		}
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Tick <= 0 {
		cfg.Tick = defaultTick
	}
	client := &http.Client{}
	if cfg.Client != nil {
		clone := *cfg.Client
		client = &clone
	}
	client.Jar = nil
	client.Timeout = fetchTimeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := &Service{cfg: cfg, client: client}
	auto, autoErr := readAuto(cfg.DataDir)
	book, err := Load(auto)
	if err != nil || autoErr != nil {
		if book, err = LoadEmbedded(); err != nil {
			return nil, err
		}
		s.loadErr = "local auto-recorded price versions could not be read; using embedded prices only"
	}
	s.book = book
	if err := readJSONFile(statePath(cfg.DataDir), &s.state); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.state = checkState{}
	}
	return s, nil
}

// Book is the current merged price book.
func (s *Service) Book() *Book {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.book
}

// AutoCheck reports the automatic-check switch (default on).
func (s *Service) AutoCheck() bool { return readAutoCheck(s.cfg.DataDir) }

// SetAutoCheck switches the automatic daily check.
func (s *Service) SetAutoCheck(on bool) error { return writeAutoCheck(s.cfg.DataDir, on) }

// Run checks the page at most once per CheckInterval while the switch is on.
// With the switch off it sends no request. It returns when ctx ends.
func (s *Service) Run(ctx context.Context) {
	delay := s.cfg.FirstDelay()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.cfg.After(delay):
		}
		delay = s.cfg.Tick
		if !s.AutoCheck() || !s.due() {
			continue
		}
		if res, err := s.Check(ctx); err != nil {
			s.cfg.Logger.Warn("price page check failed", "err", err)
		} else if res.Outcome != OutcomeUnchanged {
			s.cfg.Logger.Info("price page check", "outcome", res.Outcome, "reason", res.Reason, "recorded", res.Recorded)
		}
	}
}

func (s *Service) due() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.LastCheckAt == nil || s.cfg.Now().Sub(*s.state.LastCheckAt) >= CheckInterval
}

// CheckResult is the result of one page check.
type CheckResult struct {
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason,omitempty"`
	Recorded   string    `json:"recordedVersion,omitempty"`
	PageSHA256 string    `json:"pageSha256,omitempty"`
	FetchedAt  time.Time `json:"fetchedAt"`
	Diff       *Diff     `json:"diff,omitempty"`
	Status     Status    `json:"status"`
}

type fetched struct {
	notModified        bool
	body               []byte
	etag, lastModified string
}

// Check fetches the page now and compares it with the current version. A
// parsed, plausible change is recorded as a new version; a suspicious one is
// held for Accept; a failure keeps the last version. The returned error is
// only for local I/O failures; network and parse failures are outcomes.
func (s *Service) Check(ctx context.Context) (CheckResult, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	now := s.cfg.Now().UTC()
	s.mu.Lock()
	st := s.state
	latest := s.book.Latest()
	s.mu.Unlock()
	conditional := (st.LastCheckStatus == OutcomeUnchanged || st.LastCheckStatus == OutcomeChanged) && st.ComparedVersion == latest.Version
	res := CheckResult{FetchedAt: now}
	f, reason := s.fetch(ctx, st, conditional)
	st.LastCheckAt = &now
	switch {
	case reason != "":
		res.Outcome, res.Reason = OutcomeCannotVerify, reason
	case f.notModified:
		res.Outcome, res.PageSHA256 = OutcomeUnchanged, st.PageSHA256
	default:
		sum := sha256.Sum256(f.body)
		res.PageSHA256 = hex.EncodeToString(sum[:])
		page, err := ParsePage(f.body)
		if err != nil {
			res.Outcome, res.Reason = OutcomeCannotVerify, err.Error()
			break
		}
		diff, cand, err := Compare(latest, page, now, res.PageSHA256)
		if err != nil {
			res.Outcome, res.Reason = OutcomeCannotVerify, err.Error()
			break
		}
		res.Outcome = diff.Outcome
		if cand != nil {
			res.Diff = &diff
		}
		switch diff.Outcome {
		case OutcomeChanged:
			name, err := s.record(*cand)
			if err != nil {
				return res, err
			}
			res.Recorded = name
			st.Pending = nil
		case OutcomeSuspicious:
			if st.Pending == nil || st.Pending.PageSHA256 != res.PageSHA256 {
				st.Pending = &pending{PageSHA256: res.PageSHA256, FetchedAt: now, Diff: diff, Version: *cand}
			}
			res.Reason = "suspicious change not recorded; accept it with --accept " + res.PageSHA256
		case OutcomeUnchanged:
			st.Pending = nil
		}
		st.PageSHA256, st.ETag, st.LastModified = res.PageSHA256, f.etag, f.lastModified
	}
	st.LastCheckStatus, st.LastCheckReason = res.Outcome, res.Reason
	if res.Outcome == OutcomeUnchanged || res.Outcome == OutcomeChanged {
		st.LastSuccessAt = &now
	}
	s.mu.Lock()
	st.ComparedVersion = s.book.Latest().Version
	s.state = st
	s.mu.Unlock()
	if err := writeJSONFile(statePath(s.cfg.DataDir), st); err != nil {
		return res, err
	}
	res.Status = s.Status()
	return res, nil
}

// Accept records the held suspicious change whose page hash is sha.
func (s *Service) Accept(sha string) (CheckResult, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	s.mu.Lock()
	st := s.state
	s.mu.Unlock()
	if st.Pending == nil || st.Pending.PageSHA256 != sha {
		return CheckResult{}, errNoPending
	}
	if st.Pending.Diff.Currency != "USD" {
		return CheckResult{}, errNotUSD
	}
	name, err := s.record(st.Pending.Version)
	if err != nil {
		return CheckResult{}, err
	}
	diff := st.Pending.Diff
	res := CheckResult{Outcome: OutcomeChanged, Reason: "accepted", Recorded: name, PageSHA256: sha, FetchedAt: st.Pending.FetchedAt, Diff: &diff}
	now := s.cfg.Now().UTC()
	st.Pending = nil
	st.LastCheckStatus, st.LastCheckReason, st.LastSuccessAt = OutcomeChanged, "accepted", &now
	s.mu.Lock()
	st.ComparedVersion = s.book.Latest().Version
	s.state = st
	s.mu.Unlock()
	if err := writeJSONFile(statePath(s.cfg.DataDir), st); err != nil {
		return res, err
	}
	res.Status = s.Status()
	return res, nil
}

// record appends v to the auto versions under a fresh name and swaps in the
// rebuilt book. Previous versions are never edited.
func (s *Service) record(v Version) (string, error) {
	auto, err := readAuto(s.cfg.DataDir)
	if err != nil {
		return "", fmt.Errorf("read auto price versions: %w", err)
	}
	s.mu.Lock()
	existing := map[string]bool{}
	for _, old := range s.book.Versions() {
		existing[old.Version] = true
	}
	s.mu.Unlock()
	for _, old := range auto {
		existing[old.Version] = true
	}
	base := "deepseek-auto-" + v.EffectiveFrom.UTC().Format(time.DateOnly)
	name := base
	for i := 2; existing[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	v.Version = name
	auto = append(auto, v)
	book, err := Load(auto)
	if err != nil {
		return "", fmt.Errorf("new price version rejected: %w", err)
	}
	if err := writeJSONFile(AutoVersionsPath(s.cfg.DataDir), autoFile{SchemaVersion: 1, Versions: auto}); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.book = book
	s.mu.Unlock()
	return name, nil
}

func (s *Service) fetch(ctx context.Context, st checkState, conditional bool) (fetched, string) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, http.NoBody)
	if err != nil {
		return fetched{}, "bad request"
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/markdown, text/plain;q=0.9")
	if conditional {
		if st.ETag != "" {
			req.Header.Set("If-None-Match", st.ETag)
		}
		if st.LastModified != "" {
			req.Header.Set("If-Modified-Since", st.LastModified)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return fetched{}, "timeout"
		}
		return fetched{}, "network error"
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified && conditional:
		return fetched{notModified: true}, ""
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fetched{}, fmt.Sprintf("http status %d (redirects are refused)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fetched{}, fmt.Sprintf("http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return fetched{}, "truncated response"
	}
	if len(body) > maxPageBytes {
		return fetched{}, "page larger than 512 KB"
	}
	return fetched{body: body, etag: resp.Header.Get("ETag"), lastModified: resp.Header.Get("Last-Modified")}, ""
}

// Status is the price book's state for display.
type Status struct {
	Provider        string       `json:"provider"`
	Currency        string       `json:"currency"`
	Version         string       `json:"version"`
	Source          string       `json:"source"`
	Label           string       `json:"label,omitempty"`
	EffectiveFrom   time.Time    `json:"effectiveFrom"`
	SourcePage      string       `json:"sourcePage"`
	AutoCheck       bool         `json:"autoCheck"`
	LastCheckAt     *time.Time   `json:"lastCheckAt,omitempty"`
	LastCheckStatus string       `json:"lastCheckStatus,omitempty"`
	LastCheckReason string       `json:"lastCheckReason,omitempty"`
	LastSuccessAt   *time.Time   `json:"lastSuccessAt,omitempty"`
	Stale           bool         `json:"stale"`
	Warnings        []string     `json:"warnings"`
	Pending         *PendingInfo `json:"pending,omitempty"`
	WindowRule      string       `json:"windowRule"`
	Rates           []ModelRates `json:"rates"`
	Versions        []Version    `json:"versions"`
	HolidayYears    []YearSource `json:"holidayYears"`
}

// PendingInfo is a held suspicious change.
type PendingInfo struct {
	PageSHA256 string    `json:"pageSha256"`
	FetchedAt  time.Time `json:"fetchedAt"`
	Diff       Diff      `json:"diff"`
}

// YearSource names one transcribed holiday notice.
type YearSource struct {
	Year        int    `json:"year"`
	SourceURL   string `json:"sourceUrl"`
	PublishedAt string `json:"publishedAt"`
}

// Status reports the current version, check state, staleness and warnings.
func (s *Service) Status() Status {
	now := s.cfg.Now().UTC()
	auto := s.AutoCheck()
	s.mu.Lock()
	book, st, loadErr := s.book, s.state, s.loadErr
	s.mu.Unlock()
	latest := book.Latest()
	out := Status{
		Provider: book.Provider, Currency: book.Currency, Version: latest.Version, Source: latest.Source, Label: latest.Label,
		EffectiveFrom: latest.EffectiveFrom, SourcePage: SourcePage, AutoCheck: auto,
		LastCheckAt: st.LastCheckAt, LastCheckStatus: st.LastCheckStatus, LastCheckReason: st.LastCheckReason, LastSuccessAt: st.LastSuccessAt,
		Warnings: []string{}, WindowRule: WindowRule, Versions: book.Versions(),
	}
	out.Rates, _ = book.Rates(now)
	for _, y := range book.Calendar().Years() {
		out.HolidayYears = append(out.HolidayYears, YearSource{Year: y.Year, SourceURL: y.SourceURL, PublishedAt: y.PublishedAt})
	}
	if loadErr != "" {
		out.Warnings = append(out.Warnings, loadErr)
	}
	// The last good check is the last successful page check, or the date the
	// embedded version was last verified by hand.
	lastGood := time.Time{}
	if st.LastSuccessAt != nil {
		lastGood = *st.LastSuccessAt
	}
	for _, v := range out.Versions {
		if d, err := time.Parse(time.DateOnly, v.CheckedAt); err == nil && d.After(lastGood) {
			lastGood = d
		}
	}
	limit := staleEmbedded
	if auto {
		limit = staleAutoCheck
	}
	if now.Sub(lastGood) > limit {
		out.Stale = true
		out.Warnings = append(out.Warnings, fmt.Sprintf("prices not verified since %s", lastGood.Format(time.DateOnly)))
	}
	if st.LastCheckStatus == OutcomeCannotVerify {
		good := "never"
		if st.LastSuccessAt != nil {
			good = st.LastSuccessAt.Format(time.DateOnly)
		}
		out.Warnings = append(out.Warnings, fmt.Sprintf("price check failed: %s (last good check %s)", st.LastCheckReason, good))
	}
	if st.Pending != nil {
		out.Pending = &PendingInfo{PageSHA256: st.Pending.PageSHA256, FetchedAt: st.Pending.FetchedAt, Diff: st.Pending.Diff}
		out.Warnings = append(out.Warnings, "suspicious price change on the page was not recorded; review it and run `ao usage prices --accept "+st.Pending.PageSHA256+"`")
	}
	if year := now.In(beijing).Year(); !book.Calendar().HasYear(year) {
		out.Stale = true
		out.Warnings = append(out.Warnings, fmt.Sprintf("holiday calendar %d missing: weekday peak hours priced as peak (upper bound)", year))
	}
	if !auto {
		out.Warnings = append(out.Warnings, "automatic price check is off")
	}
	return out
}

func readAuto(dataDir string) ([]Version, error) {
	var f autoFile
	if err := readJSONFile(AutoVersionsPath(dataDir), &f); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if f.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported auto versions schema %d", f.SchemaVersion)
	}
	return f.Versions, nil
}

func readAutoCheck(dataDir string) bool {
	var cfg struct {
		Pricebook *struct {
			AutoCheck *bool `json:"autoCheck"`
		} `json:"pricebook"`
	}
	data, err := os.ReadFile(ConfigPath(dataDir))
	if err != nil || json.Unmarshal(data, &cfg) != nil || cfg.Pricebook == nil || cfg.Pricebook.AutoCheck == nil {
		return true
	}
	return *cfg.Pricebook.AutoCheck
}

func writeAutoCheck(dataDir string, on bool) error {
	cfg := map[string]json.RawMessage{}
	if data, err := os.ReadFile(ConfigPath(dataDir)); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("provider-usage config is not valid JSON: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	section := map[string]json.RawMessage{}
	if raw, ok := cfg["pricebook"]; ok {
		if err := json.Unmarshal(raw, &section); err != nil {
			return fmt.Errorf("provider-usage config pricebook section: %w", err)
		}
	}
	section["autoCheck"], _ = json.Marshal(on)
	cfg["pricebook"], _ = json.Marshal(section)
	return writeJSONFile(ConfigPath(dataDir), cfg)
}

func readJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrict(data, out)
}

// writeJSONFile writes atomically with owner-only permissions.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
