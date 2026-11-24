package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
)

// forkUsage is the fork-only (Icosa2050, see FORK.md) wiring point for
// API-billed usage. PR A runs the DeepSeek price book: the daily keyless
// price-page check (default on, switchable with `ao usage prices auto off`)
// and the /api/v1/usage/fork routes. It plugs in through the
// `Fork: usage seam` markers in daemon.go only.
type forkUsage struct {
	svc    *pricebook.Service
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type forkUsageConfig struct {
	DataDir string
	Logger  *slog.Logger
	// Client, FirstDelay and Tick are test overrides; zero values are the
	// production defaults (http.DefaultTransport, 5-30 min jitter, 1 h).
	Client     *http.Client
	FirstDelay func() time.Duration
	Tick       time.Duration
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
	f := &forkUsage{svc: svc, cancel: cancel}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		svc.Run(runCtx)
	}()
	return f
}

// routes returns the fork usage route mounts for APIDeps.Extensions.
func (f *forkUsage) routes() []func(chi.Router) {
	if f == nil || f.svc == nil {
		return nil
	}
	return []func(chi.Router){pricebook.Routes(f.svc)}
}

// stop ends the price-check loop and waits for it.
func (f *forkUsage) stop() {
	if f == nil || f.cancel == nil {
		return
	}
	f.cancel()
	f.wg.Wait()
}
