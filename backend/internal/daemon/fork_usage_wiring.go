package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/providers"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/reportext"
)

// forkUsage is the fork-only (Icosa2050, see FORK.md) wiring point for
// API-billed usage. It runs the DeepSeek price book (the daily keyless
// price-page check, default on, switchable with `ao usage prices auto off`),
// the read-time DeepSeek report extension (/api/v1/usage/fork/report-ext),
// the opt-in provider polling (DeepSeek balance, OpenRouter key; default off,
// `ao usage providers enable`) and the /api/v1/usage/fork routes. It plugs in
// through the `Fork: usage seam` markers in daemon.go only.
type forkUsage struct {
	svc       *pricebook.Service
	store     reportext.Store
	providers *providers.Service
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

type forkUsageConfig struct {
	DataDir string
	Logger  *slog.Logger
	// Store backs report-ext; nil leaves that route unmounted.
	Store reportext.Store
	// Client, FirstDelay and Tick are test overrides; zero values are the
	// production defaults (http.DefaultTransport, 5-30 min jitter, 1 h).
	Client     *http.Client
	FirstDelay func() time.Duration
	Tick       time.Duration
	// Provider polling test overrides: key reader and loop tick.
	Keys         providers.KeyReader
	ProviderTick time.Duration
}

// startForkUsage starts the price book. A failure disables the fork usage
// routes and is logged; it never stops the daemon.
func startForkUsage(ctx context.Context, cfg forkUsageConfig) *forkUsage {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	svc, err := pricebook.NewService(pricebook.Config{
		DataDir: cfg.DataDir, Client: cfg.Client, Logger: log, FirstDelay: cfg.FirstDelay, Tick: cfg.Tick,
	})
	if err != nil {
		log.Warn("fork usage price book disabled", "err", err)
		return &forkUsage{}
	}
	runCtx, cancel := context.WithCancel(ctx)
	f := &forkUsage{svc: svc, store: cfg.Store, cancel: cancel}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		svc.Run(runCtx)
	}()
	prov, err := providers.NewService(providers.Config{
		DataDir: cfg.DataDir, Client: cfg.Client, Keys: cfg.Keys, Logger: log, Tick: cfg.ProviderTick, Window: svc.WindowReport,
	})
	if err != nil {
		log.Warn("fork usage provider polling disabled", "err", err)
		return f
	}
	f.providers = prov
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		prov.Run(runCtx)
	}()
	return f
}

// routes returns the fork usage route mounts for APIDeps.Extensions.
func (f *forkUsage) routes() []func(chi.Router) {
	if f == nil || f.svc == nil {
		return nil
	}
	mounts := []func(chi.Router){pricebook.Routes(f.svc)}
	if f.store != nil {
		mounts = append(mounts, reportext.Routes(&reportext.Service{Store: f.store, Prices: f.svc}))
	}
	if f.providers != nil {
		mounts = append(mounts, providers.Routes(f.providers))
	}
	return mounts
}

// stop ends the price-check and provider loops and waits for them.
func (f *forkUsage) stop() {
	if f == nil || f.cancel == nil {
		return
	}
	f.cancel()
	f.wg.Wait()
}
