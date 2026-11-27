package providers

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
)

// Cadence.
const (
	// PollInterval is the DeepSeek balance cadence while enabled.
	PollInterval = 15 * time.Minute
	// FreshFor is how old a reading may be before a status read refreshes it
	// (DeepSeek on-demand refresh, OpenRouter cache).
	FreshFor    = 5 * time.Minute
	maxBackoff  = time.Hour
	defaultTick = time.Minute
)

// SchemaVersion is the `providersSchema` of the status answer.
const SchemaVersion = 1

// Config wires a Service. Zero values are production defaults.
type Config struct {
	DataDir string
	// Client sends provider requests; nil uses http.DefaultTransport. Its
	// jar, redirect policy and timeout are overridden.
	Client *http.Client
	Keys   KeyReader // default SystemKeys{}
	// Window answers the DeepSeek peak/off-peak window (the price book);
	// nil leaves the window out.
	Window func(time.Time) pricebook.WindowReport
	Now    func() time.Time
	Logger *slog.Logger
	// Test overrides: endpoint URLs (must be https), request timeout, loop
	// tick and backoff jitter.
	DeepSeekURL   string
	OpenRouterURL string
	ModelsURL     string
	// PAL locates the PAL MCP config (allowed models, preset chains).
	PAL     PALConfigPaths
	Timeout time.Duration
	Tick    time.Duration
	After   func(time.Duration) <-chan time.Time
	Jitter  func(time.Duration) time.Duration
}

type pollState struct {
	mu           sync.Mutex // serializes fetches of this provider
	lastAttempt  time.Time
	backoffUntil time.Time
	failures     int
	reason       string
	fetchedAt    *time.Time
	keyID        string
	// DeepSeek
	available bool
	balances  []Balance
	// OpenRouter
	key *KeyInfo
}

// Service polls the enabled providers and answers their status.
type Service struct {
	cfg    Config
	client *http.Client
	// public sends the keyless models-listing request.
	public    *http.Client
	listingMu sync.Mutex // serializes listing fetches
	mu        sync.Mutex // guards the pollState fields below the fetch mutex
	state     map[string]*pollState
}

// NewService validates cfg.
func NewService(cfg Config) (*Service, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("providers: data dir required")
	}
	if cfg.DeepSeekURL == "" {
		cfg.DeepSeekURL = DeepSeekBalanceURL
	}
	if cfg.OpenRouterURL == "" {
		cfg.OpenRouterURL = OpenRouterKeyURL
	}
	if cfg.ModelsURL == "" {
		cfg.ModelsURL = OpenRouterModelsURL
	}
	for _, raw := range []string{cfg.DeepSeekURL, cfg.OpenRouterURL, cfg.ModelsURL} {
		if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, errors.New("providers: endpoint URLs must be https")
		}
	}
	if cfg.Keys == nil {
		cfg.Keys = SystemKeys{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = fetchTimeout
	}
	if cfg.Tick <= 0 {
		cfg.Tick = defaultTick
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Jitter == nil {
		cfg.Jitter = func(d time.Duration) time.Duration {
			return time.Duration(rand.Int64N(int64(d)/5 + 1)) //nolint:gosec // scheduling jitter, not security
		}
	}
	listingTimeoutFor := listingTimeout
	if cfg.Timeout < fetchTimeout {
		listingTimeoutFor = cfg.Timeout // tests shorten every timeout
	}
	s := &Service{cfg: cfg, client: newClient(cfg.Client, cfg.Timeout), public: newClient(cfg.Client, listingTimeoutFor), state: map[string]*pollState{}}
	for _, p := range Known {
		s.state[p] = &pollState{}
	}
	return s, nil
}

// Enabled reports the opt-in switches (default all off).
func (s *Service) Enabled() map[string]bool {
	on, _ := readConfig(s.cfg.DataDir)
	return on
}

// SetEnabled switches one provider. Enabling clears any error backoff so a
// fixed key is tried at once; disabling drops the in-memory reading (the
// balance history file stays).
func (s *Service) SetEnabled(provider string, on bool) error {
	if err := writeEnabled(s.cfg.DataDir, provider, on); err != nil {
		return err
	}
	st := s.state[provider]
	st.mu.Lock()
	defer st.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		st.backoffUntil, st.failures, st.lastAttempt = time.Time{}, 0, time.Time{}
	} else {
		st.lastAttempt, st.backoffUntil, st.failures, st.reason = time.Time{}, time.Time{}, 0, ""
		st.fetchedAt, st.keyID, st.available, st.balances, st.key = nil, "", false, nil, nil
	}
	return nil
}

// Run polls the DeepSeek balance every PollInterval while it is enabled
// (balance changes need history). OpenRouter's key is read on demand only.
// The keyless OpenRouter model listing is refreshed at most daily while its
// switch is on (default on). With every provider and the listing off it
// sends nothing. It returns when ctx ends.
func (s *Service) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.cfg.After(s.cfg.Tick):
		}
		enabled, _, listing := readSettings(s.cfg.DataDir)
		if listing {
			s.refreshListingIfDue(ctx)
		}
		if !enabled[DeepSeek] {
			continue
		}
		s.refreshIfDue(ctx, DeepSeek, PollInterval)
	}
}

// refreshListingIfDue fetches the public model listing when the cached one
// is older than ListingEvery and the last attempt is older than an hour.
// Attempts are persisted, so restarts do not refetch.
func (s *Service) refreshListingIfDue(ctx context.Context) {
	s.listingMu.Lock()
	defer s.listingMu.Unlock()
	if _, _, on := readSettings(s.cfg.DataDir); !on {
		return
	}
	now := s.cfg.Now().UTC()
	cached, _ := readListing(s.cfg.DataDir)
	if (len(cached.Models) > 0 && now.Sub(cached.FetchedAt) < ListingEvery) || now.Sub(cached.LastAttempt) < listingRetry {
		return
	}
	var resp openrouterModelsResponse
	reason := getPublicJSON(ctx, s.public, s.cfg.ModelsURL, maxListingBytes, &resp)
	cached.LastAttempt = now
	if reason == "" {
		models, ok := parseListing(resp)
		if ok {
			cached.Models, cached.FetchedAt, cached.LastReason = models, now, ""
		} else {
			reason = ReasonUnexpected
		}
	}
	if reason != "" {
		cached.LastReason = reason
		s.cfg.Logger.Info("openrouter model listing fetch failed", "reason", reason)
	}
	if err := writeListing(s.cfg.DataDir, cached); err != nil {
		s.cfg.Logger.Warn("provider usage: could not save the OpenRouter model listing")
	}
}

// refreshIfDue fetches provider when its last attempt is older than every
// and it is not backing off after an error.
func (s *Service) refreshIfDue(ctx context.Context, provider string, every time.Duration) {
	st := s.state[provider]
	st.mu.Lock()
	defer st.mu.Unlock()
	// Re-check the opt-in under the fetch lock: SetEnabled(false) takes this
	// lock after saving the switch, so a refresh that was waiting here while
	// the provider was disabled must not send the key.
	if !s.Enabled()[provider] {
		return
	}
	now := s.cfg.Now()
	s.mu.Lock()
	due := now.Sub(st.lastAttempt) >= every && !now.Before(st.backoffUntil)
	s.mu.Unlock()
	if !due {
		return
	}
	s.fetch(ctx, provider, now)
}

// fetch reads provider once. Must hold st.mu. The key lives only in this
// call's stack.
func (s *Service) fetch(ctx context.Context, provider string, now time.Time) {
	st := s.state[provider]
	reason, keyID := "", ""
	var balances []Balance
	var available bool
	var key *KeyInfo
	func() {
		k, err := s.cfg.Keys.Key(ctx, provider)
		if err != nil {
			reason = keyReason(err)
			return
		}
		keyID = KeyID(k)
		switch provider {
		case DeepSeek:
			var resp deepseekBalanceResponse
			if reason = getJSON(ctx, s.client, s.cfg.DeepSeekURL, k, &resp); reason != "" {
				return
			}
			var ok bool
			if available, balances, ok = parseBalance(resp); !ok {
				reason = ReasonUnexpected
			}
		case OpenRouter:
			var resp openrouterKeyResponse
			if reason = getJSON(ctx, s.client, s.cfg.OpenRouterURL, k, &resp); reason != "" {
				return
			}
			info, ok := parseKey(resp, now)
			if !ok {
				reason = ReasonUnexpected
				return
			}
			key = &info
		}
	}()
	if reason == "" && provider == DeepSeek {
		add := make([]Sample, 0, len(balances))
		for _, b := range balances {
			add = append(add, Sample{TS: now.UTC(), KeyID: keyID, Currency: b.Currency, Total: b.Total, Granted: b.Granted, ToppedUp: b.ToppedUp})
		}
		if err := appendSamples(s.cfg.DataDir, now, add); err != nil {
			s.cfg.Logger.Warn("provider usage: could not save the DeepSeek balance sample", "provider", provider)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.lastAttempt = now
	if reason != "" {
		st.failures++
		backoff := PollInterval << min(st.failures-1, 2)
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		st.backoffUntil = now.Add(backoff + s.cfg.Jitter(backoff))
		st.reason = reason
		if st.keyID != "" && keyID != "" && st.keyID != keyID {
			// Another key: the old reading belongs to it, not this one.
			st.fetchedAt, st.balances, st.key = nil, nil, nil
		}
		if keyID != "" {
			st.keyID = keyID
		}
		s.cfg.Logger.Info("provider usage fetch failed", "provider", provider, "reason", reason)
		return
	}
	t := now.UTC()
	st.failures, st.backoffUntil, st.reason, st.fetchedAt, st.keyID = 0, time.Time{}, "", &t, keyID
	st.available, st.balances, st.key = available, balances, key
}

// Row is one provider's status.
type Row struct {
	Provider  string     `json:"provider"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	KeyID     string     `json:"keyId,omitempty"`
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
	// Stale is true when the reading shown is older than the last failed
	// attempt.
	Stale bool `json:"stale,omitempty"`
	// DeepSeek
	IsAvailable *bool      `json:"isAvailable,omitempty"`
	Balance     *Balance   `json:"balance,omitempty"`
	Balances    []Balance  `json:"balances,omitempty"`
	Change      *Change    `json:"change,omitempty"`
	OutsideAO   *OutsideAO `json:"outsideAo,omitempty"`
	Window      *Window    `json:"window,omitempty"`
	PriceBook   *BookRef   `json:"priceBook,omitempty"`
	// OpenRouter
	Key *KeyInfo `json:"key,omitempty"`
	// Hosts is set by the fleet view: the hosts that report this key.
	Hosts []string `json:"hosts,omitempty"`
}

// Window is the DeepSeek peak/off-peak window now (keyless, local).
type Window struct {
	Current         string     `json:"current"`
	PriceFactor     string     `json:"priceFactor,omitempty"`
	PeakVsOffPeak   string     `json:"peakVsOffPeak,omitempty"`
	Until           *time.Time `json:"until,omitempty"`
	NextOffPeak     *Span      `json:"nextOffPeak,omitempty"`
	NextPeak        *Span      `json:"nextPeak,omitempty"`
	Holiday         string     `json:"holiday,omitempty"`
	Rule            string     `json:"rule"`
	HolidayCalendar string     `json:"holidayCalendar"`
}

// Span is a UTC interval.
type Span struct {
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
}

// BookRef names the price book behind the window.
type BookRef struct {
	Version     string     `json:"version"`
	LastCheckAt *time.Time `json:"lastCheckAt,omitempty"`
	Stale       bool       `json:"stale"`
	Warnings    []string   `json:"warnings,omitempty"`
}

// StatusResponse is GET /api/v1/usage/fork/providers.
type StatusResponse struct {
	ProvidersSchema int        `json:"providersSchema"`
	Providers       []Row      `json:"providers"`
	Thresholds      Thresholds `json:"thresholds"`
	// ModelsListing is the OpenRouter model-listing switch (default on).
	ModelsListing bool `json:"modelsListing"`
	// Steering is the steering block (design §9); absent from daemons that
	// predate it.
	Steering *Steering `json:"steering,omitempty"`
}

// Status answers every provider. With refresh, enabled providers whose
// reading is older than FreshFor (and that are not backing off) are fetched
// first, concurrently.
func (s *Service) Status(ctx context.Context, refresh bool) StatusResponse {
	enabled, th, listing := readSettings(s.cfg.DataDir)
	if refresh {
		// A client hanging up must not turn into a provider error and a
		// backoff; the client's own timeout bounds the fetch.
		ctx = context.WithoutCancel(ctx)
		var wg sync.WaitGroup
		for _, p := range Known {
			if enabled[p] {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.refreshIfDue(ctx, p, FreshFor)
				}()
			}
		}
		wg.Wait()
	}
	now := s.cfg.Now()
	out := StatusResponse{ProvidersSchema: SchemaVersion, Thresholds: th, ModelsListing: listing, Providers: make([]Row, 0, len(Known))}
	for _, p := range Known {
		out.Providers = append(out.Providers, s.row(p, enabled[p], now))
	}
	in := steerInputs{now: now, th: th, listingOn: listing, pal: readPALConfig(s.cfg.PAL)}
	for _, r := range out.Providers {
		switch r.Provider {
		case DeepSeek:
			in.deepseek = r
		case OpenRouter:
			in.openrouter = r
		}
	}
	if s.cfg.Window != nil {
		w := s.cfg.Window(now)
		in.window = &w
	}
	if enabled[DeepSeek] {
		samples, err := readSamples(s.cfg.DataDir)
		in.samples, in.samplesErr = samples, err != nil
	}
	in.listing, in.listingOK = readListing(s.cfg.DataDir)
	st := buildSteering(in)
	out.Steering = &st
	return out
}

// SetSettings updates the steering thresholds or the listing switch.
func (s *Service) SetSettings(set Settings) error { return writeSettings(s.cfg.DataDir, set) }

func (s *Service) row(provider string, enabled bool, now time.Time) Row {
	r := Row{Provider: provider, Status: StatusDisabled}
	if provider == DeepSeek && s.cfg.Window != nil {
		r.Window, r.PriceBook = windowOf(s.cfg.Window(now))
	}
	if !enabled {
		r.Reason = ReasonDisabled + "; enable with `ao usage providers enable " + provider + "`"
		return r
	}
	s.mu.Lock()
	st := s.state[provider]
	reason, fetchedAt, keyID := st.reason, st.fetchedAt, st.keyID
	available, balances, key := st.available, append([]Balance(nil), st.balances...), st.key
	s.mu.Unlock()
	r.KeyID, r.FetchedAt = keyID, fetchedAt
	switch {
	case reason != "":
		r.Status, r.Reason, r.Stale = StatusUnavailable, reason, fetchedAt != nil
	case fetchedAt == nil:
		r.Status, r.Reason = StatusUnavailable, "not read yet"
		return r
	default:
		r.Status = StatusOK
	}
	if fetchedAt == nil {
		return r
	}
	switch provider {
	case DeepSeek:
		if len(balances) == 0 {
			return r
		}
		r.IsAvailable = &available
		primary := primaryBalance(balances)
		r.Balance = &primary
		if len(balances) > 1 {
			r.Balances = balances
		}
		samples, err := readSamples(s.cfg.DataDir)
		var ch Change
		if err != nil {
			ch = Change{Window: "24h", Currency: primary.Currency, Reason: "balance history unreadable", Adjustments: []Adjustment{}}
		} else {
			ch = computeChange(samples, keyID, primary.Currency, now)
		}
		o := outsideAO(ch)
		r.Change, r.OutsideAO = &ch, &o
	case OpenRouter:
		r.Key = key
	}
	return r
}

func windowOf(rep pricebook.WindowReport) (*Window, *BookRef) {
	w := &Window{
		Current: rep.Window, PriceFactor: rep.PriceFactor, PeakVsOffPeak: rep.PeakVsOffPeak, Holiday: rep.Holiday,
		Rule: rep.WindowRule, HolidayCalendar: rep.HolidayCalendar,
	}
	if rep.WindowEndsAt != nil {
		t := rep.WindowEndsAt.UTC
		w.Until = &t
	}
	if rep.NextOffPeak != nil {
		w.NextOffPeak = &Span{From: rep.NextOffPeak.From.UTC, Until: rep.NextOffPeak.Until.UTC}
	}
	if rep.NextPeak != nil {
		w.NextPeak = &Span{From: rep.NextPeak.From.UTC, Until: rep.NextPeak.Until.UTC}
	}
	b := &BookRef{Version: rep.PriceBook.Version, LastCheckAt: rep.PriceBook.LastCheckAt, Stale: rep.PriceBook.Stale, Warnings: rep.PriceBook.Warnings}
	return w, b
}
