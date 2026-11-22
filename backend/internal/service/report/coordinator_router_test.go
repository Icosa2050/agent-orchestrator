package report

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Fork: fleet seam tests for CoordinatorDeps.Router (FORK.md, test-plan U-Q4.1–U-Q4.5).

type routerCall struct {
	project domain.ProjectID
	ids     []string
	states  []domain.ReportDeliveryState
	// lookups is how many orchestrator lookups (ListSessions) ran before Route.
	lookups int
}

type fakeReportRouter struct {
	mu      sync.Mutex
	store   *countingStore
	handled func(domain.ProjectID) bool
	err     error
	calls   []routerCall
	mutate  bool
}

func (r *fakeReportRouter) Route(_ context.Context, project domain.ProjectID, batch []domain.ReportRecord) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := routerCall{project: project}
	if r.store != nil {
		call.lookups = r.store.lookups()
	}
	for i := range batch {
		call.ids = append(call.ids, batch[i].ID)
		call.states = append(call.states, batch[i].DeliveryState)
		if r.mutate {
			batch[i].Note = "mutated by router"
			for j := range batch[i].Outputs {
				batch[i].Outputs[j].Reference = "mutated-output"
			}
		}
	}
	r.calls = append(r.calls, call)
	if r.err != nil {
		return false, r.err
	}
	return r.handled != nil && r.handled(project), nil
}

// countingStore counts orchestrator lookups so tests can prove Route runs first.
type countingStore struct {
	*coordinatorStore
	mu   sync.Mutex
	list int
}

func (s *countingStore) ListSessions(ctx context.Context, project domain.ProjectID) ([]domain.SessionRecord, error) {
	s.mu.Lock()
	s.list++
	s.mu.Unlock()
	return s.coordinatorStore.ListSessions(ctx, project)
}

func (s *countingStore) lookups() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list
}

func always(domain.ProjectID) bool { return true }
func never(domain.ProjectID) bool  { return false }

type coordinatorOutcome struct {
	Submissions []coordinatorSubmission
	Interrupts  []domain.SessionID
	Acked       int
	States      map[string]domain.ReportDeliveryState
	Errors      []string
	Delays      []time.Duration
}

// runCoordinatorScenarios replays the existing local-delivery scenarios with
// the given router and records everything observable.
func runCoordinatorScenarios(t *testing.T, router func(*countingStore) Router) []coordinatorOutcome {
	t.Helper()
	type scenario struct {
		setup func(*coordinatorStore, time.Time)
		runs  int
	}
	stuck := func(id string, at time.Time) domain.ReportRecord {
		r := reportAt(id, "worker", at, at, "cannot continue")
		r.State = domain.ReportStuck
		return r
	}
	scenarios := []scenario{
		{runs: 2, setup: func(s *coordinatorStore, now time.Time) { // fallback batch
			s.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
			s.add(reportAt("one", "worker", now, now, "first"), reportAt("two", "worker", now.Add(time.Minute), now.Add(time.Hour), "second"))
		}},
		{runs: 2, setup: func(s *coordinatorStore, now time.Time) { // stuck interrupt + coalescing
			s.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
			s.add(stuck("stuck-1", now), stuck("stuck-2", now.Add(time.Second)))
		}},
		{runs: 1, setup: func(s *coordinatorStore, now time.Time) { // terminal orchestrator never interrupted
			s.sessions = []domain.SessionRecord{{ID: "term", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeTUI}}
			s.add(stuck("stuck-t", now))
		}},
		{runs: 1, setup: func(s *coordinatorStore, now time.Time) { // newest live orchestrator wins
			s.sessions = []domain.SessionRecord{
				{ID: "old", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat, IsTerminated: true, CreatedAt: now},
				{ID: "replacement", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat, CreatedAt: now.Add(time.Second)},
			}
			s.add(reportAt("need", "worker", now, now, "decision"))
		}},
	}
	var out []coordinatorOutcome
	for _, sc := range scenarios {
		now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
		base := newCoordinatorStore(now)
		sc.setup(base, now)
		store := &countingStore{coordinatorStore: base}
		delivery := &coordinatorDelivery{}
		c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: delivery, Now: func() time.Time { return now }, NewToken: func() string { return "claim" }, RetryDelay: time.Second, Router: router(store)})
		var o coordinatorOutcome
		for i := 0; i < sc.runs; i++ {
			delay, err := c.RunDue(context.Background())
			o.Delays = append(o.Delays, delay)
			if err != nil {
				o.Errors = append(o.Errors, err.Error())
			}
			now = now.Add(time.Hour)
		}
		o.Submissions, o.Interrupts, o.Acked = delivery.submissions, delivery.interrupts, base.acked
		o.States = map[string]domain.ReportDeliveryState{}
		for _, r := range base.reports {
			o.States[r.ID] = r.DeliveryState
		}
		out = append(out, o)
	}
	return out
}

// U-Q4.1 / U-Q4.3: a nil router and a router that never handles leave every
// existing local-delivery scenario byte-for-byte unchanged.
func TestForkReportRouterUnhandledKeepsLocalDeliveryIdentical(t *testing.T) {
	baseline := runCoordinatorScenarios(t, func(*countingStore) Router { return nil })
	if len(baseline[0].Submissions) == 0 || len(baseline[1].Interrupts) != 1 {
		t.Fatalf("baseline scenarios did not exercise delivery: %#v", baseline)
	}
	var routers []*fakeReportRouter
	unhandled := runCoordinatorScenarios(t, func(s *countingStore) Router {
		r := &fakeReportRouter{handled: never, store: s}
		routers = append(routers, r)
		return r
	})
	if !reflect.DeepEqual(baseline, unhandled) {
		t.Fatalf("unhandled router changed local delivery\nnil:       %#v\nunhandled: %#v", baseline, unhandled)
	}
	for i, r := range routers {
		if len(r.calls) == 0 {
			t.Fatalf("scenario %d: router never consulted", i)
		}
	}
}

// U-Q4.1: the nil router never claims a batch it cannot deliver (today's
// lookup-before-claim order), so reports keep their scheduling untouched.
func TestForkReportRouterNilKeepsLookupBeforeClaim(t *testing.T) {
	now := time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC)
	store := newCoordinatorStore(now)
	store.add(reportAt("need", "worker", now, now, "decision"))
	c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: &coordinatorDelivery{}, Now: func() time.Time { return now }, RetryDelay: time.Second})
	if _, err := c.RunDue(context.Background()); err == nil || !strings.Contains(err.Error(), "no active orchestrator") {
		t.Fatalf("err = %v", err)
	}
	if got := store.reports[0]; got.DeliveryState != domain.ReportPending || got.ClaimToken != "" || got.LastError != "" || got.DeliveryBatchID != "" {
		t.Fatalf("nil router touched the report: %#v", got)
	}
}

// U-Q4.2: a handled batch is acknowledged with zero Submit and zero Interrupt,
// even for an urgent stuck report, and with no local orchestrator lookup.
func TestForkReportRouterHandledAcksWithoutLocalWake(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, withOrchestrator := range []bool{true, false} {
		base := newCoordinatorStore(now)
		if withOrchestrator {
			base.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
		}
		stuck := reportAt("stuck", "worker", now, now, "cannot continue")
		stuck.State = domain.ReportStuck
		base.add(stuck, reportAt("later", "worker", now.Add(time.Second), now.Add(time.Hour), "context"))
		store := &countingStore{coordinatorStore: base}
		router := &fakeReportRouter{handled: always, store: store}
		delivery := &coordinatorDelivery{}
		c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: delivery, Now: func() time.Time { return now }, NewToken: func() string { return "claim" }, Router: router})
		delay, err := c.RunDue(context.Background())
		if err != nil || delay != -1 {
			t.Fatalf("orchestrator=%v: delay=%v err=%v", withOrchestrator, delay, err)
		}
		if len(delivery.submissions) != 0 || len(delivery.interrupts) != 0 {
			t.Fatalf("orchestrator=%v: local wake: submissions=%v interrupts=%v", withOrchestrator, delivery.submissions, delivery.interrupts)
		}
		if store.lookups() != 0 {
			t.Fatalf("orchestrator=%v: %d local orchestrator lookups", withOrchestrator, store.lookups())
		}
		if base.acked != 2 {
			t.Fatalf("orchestrator=%v: acked=%d, want 2", withOrchestrator, base.acked)
		}
		if len(router.calls) != 1 {
			t.Fatalf("orchestrator=%v: router calls %#v", withOrchestrator, router.calls)
		}
		call := router.calls[0]
		// Route sees the whole claimed batch, already claimed, before any lookup.
		if call.project != "p" || !reflect.DeepEqual(call.ids, []string{"stuck", "later"}) || call.lookups != 0 {
			t.Fatalf("orchestrator=%v: call %#v", withOrchestrator, call)
		}
		for _, st := range call.states {
			if st != domain.ReportClaimed {
				t.Fatalf("orchestrator=%v: Route saw an unclaimed report: %v", withOrchestrator, call.states)
			}
		}
	}
}

// U-Q4.4: a router error releases the batch for a retry: no acknowledgement,
// no Submit, no Interrupt; the next run routes it again.
func TestForkReportRouterErrorReleasesForRetry(t *testing.T) {
	now := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	base := newCoordinatorStore(now)
	base.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	stuck := reportAt("stuck", "worker", now, now, "cannot continue")
	stuck.State = domain.ReportStuck
	base.add(stuck)
	router := &fakeReportRouter{err: errors.New("grants unavailable")}
	delivery := &coordinatorDelivery{}
	c := NewCoordinator(CoordinatorDeps{Store: base, Delivery: delivery, Now: func() time.Time { return now }, RetryDelay: 7 * time.Second, Router: router})
	delay, err := c.RunDue(context.Background())
	if err == nil || !strings.Contains(err.Error(), "grants unavailable") || delay != 7*time.Second {
		t.Fatalf("delay=%v err=%v", delay, err)
	}
	if got := base.reports[0]; got.DeliveryState != domain.ReportPending || got.ClaimToken != "" || !strings.Contains(got.LastError, "route reports: grants unavailable") {
		t.Fatalf("report after router error: %#v", got)
	}
	if base.acked != 0 || len(delivery.submissions) != 0 || len(delivery.interrupts) != 0 {
		t.Fatalf("acked=%d submissions=%v interrupts=%v", base.acked, delivery.submissions, delivery.interrupts)
	}
	router.mu.Lock()
	router.err, router.handled = nil, always
	router.mu.Unlock()
	if _, err := c.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if base.acked != 1 || len(router.calls) != 2 || len(delivery.submissions) != 0 {
		t.Fatalf("retry: acked=%d calls=%d submissions=%v", base.acked, len(router.calls), delivery.submissions)
	}
}

// An unrouted batch with no local orchestrator is released, not left claimed,
// and nothing is submitted.
func TestForkReportRouterUnhandledWithoutOrchestratorReleases(t *testing.T) {
	now := time.Date(2026, 9, 14, 14, 0, 0, 0, time.UTC)
	base := newCoordinatorStore(now)
	base.add(reportAt("need", "worker", now, now, "decision"))
	delivery := &coordinatorDelivery{}
	c := NewCoordinator(CoordinatorDeps{Store: base, Delivery: delivery, Now: func() time.Time { return now }, RetryDelay: time.Second, Router: &fakeReportRouter{handled: never}})
	if _, err := c.RunDue(context.Background()); err == nil || !strings.Contains(err.Error(), "no active orchestrator") {
		t.Fatalf("err = %v", err)
	}
	if got := base.reports[0]; got.DeliveryState != domain.ReportPending || got.ClaimToken != "" {
		t.Fatalf("report left claimed: %#v", got)
	}
	if len(delivery.submissions) != 0 || base.acked != 0 {
		t.Fatalf("submissions=%v acked=%d", delivery.submissions, base.acked)
	}
}

// Route gets a copy: a router that edits its batch cannot change what the
// local orchestrator receives.
func TestForkReportRouterCannotMutateLocalBatch(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC)
	base := newCoordinatorStore(now)
	base.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	info := reportAt("info", "worker", now, now, "original note")
	info.Outputs = []domain.ReportOutput{{Kind: domain.ReportOutputPRCreated, Reference: "https://github.com/o/r/pull/7", Label: "pr"}}
	base.add(info)
	delivery := &coordinatorDelivery{}
	c := NewCoordinator(CoordinatorDeps{Store: base, Delivery: delivery, Now: func() time.Time { return now }, Router: &fakeReportRouter{handled: never, mutate: true}})
	if _, err := c.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivery.submissions) != 1 || !strings.Contains(delivery.submissions[0].message, "original note") || !strings.Contains(delivery.submissions[0].message, "pull/7") || strings.Contains(delivery.submissions[0].message, "mutated") {
		t.Fatalf("submissions = %#v", delivery.submissions)
	}
}

// U-Q4.5: a local Chat turn never piggybacks routed reports.
func TestForkReportRouterPiggyback(t *testing.T) {
	now := time.Date(2026, 9, 14, 16, 0, 0, 0, time.UTC)
	setup := func(router Router) (*coordinatorStore, *Coordinator) {
		base := newCoordinatorStore(now)
		base.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
		base.add(reportAt("info", "worker", now, now.Add(time.Hour), "context"))
		return base, NewCoordinator(CoordinatorDeps{Store: base, Delivery: &coordinatorDelivery{}, Now: func() time.Time { return now }, NewToken: func() string { return "piggyback-token" }, Router: router})
	}

	t.Run("handled", func(t *testing.T) {
		router := &fakeReportRouter{handled: always}
		store, c := setup(router)
		batch, err := c.PreparePiggyback(context.Background(), "orch")
		if err != nil || len(batch.Reports) != 0 || batch.AppendToUserMessage("hello") != "hello" {
			t.Fatalf("batch=%#v err=%v", batch, err)
		}
		if store.acked != 1 || store.reports[0].DeliveryState != domain.ReportAcknowledged || len(router.calls) != 1 {
			t.Fatalf("acked=%d state=%s calls=%d", store.acked, store.reports[0].DeliveryState, len(router.calls))
		}
	})
	t.Run("router error appends nothing and keeps the message", func(t *testing.T) {
		store, c := setup(&fakeReportRouter{err: errors.New("route down")})
		batch, err := c.PreparePiggyback(context.Background(), "orch")
		if err != nil || len(batch.Reports) != 0 {
			t.Fatalf("batch=%#v err=%v", batch, err)
		}
		if got := store.reports[0]; got.DeliveryState != domain.ReportPending || store.acked != 0 || !strings.Contains(got.LastError, "route down") {
			t.Fatalf("report=%#v acked=%d", got, store.acked)
		}
	})
	t.Run("unhandled matches nil router", func(t *testing.T) {
		_, plain := setup(nil)
		want, err := plain.PreparePiggyback(context.Background(), "orch")
		if err != nil {
			t.Fatal(err)
		}
		store, c := setup(&fakeReportRouter{handled: never})
		got, err := c.PreparePiggyback(context.Background(), "orch")
		if err != nil || !reflect.DeepEqual(got, want) || store.acked != 0 {
			t.Fatalf("got=%#v want=%#v err=%v acked=%d", got, want, err, store.acked)
		}
	})
	t.Run("store failures after routing never fail the message", func(t *testing.T) {
		for _, router := range []*fakeReportRouter{{handled: always}, {err: errors.New("route down")}} {
			base, _ := setup(nil)
			store := &failingAckStore{coordinatorStore: base}
			c := NewCoordinator(CoordinatorDeps{Store: store, Delivery: &coordinatorDelivery{}, Now: func() time.Time { return now }, Router: router})
			batch, err := c.PreparePiggyback(context.Background(), "orch")
			if err != nil || len(batch.Reports) != 0 {
				t.Fatalf("batch=%#v err=%v", batch, err)
			}
			// Left claimed for RequeueClaimedReports at daemon start; never piggybacked.
			if base.reports[0].DeliveryState != domain.ReportClaimed || base.acked != 0 {
				t.Fatalf("report=%#v acked=%d", base.reports[0], base.acked)
			}
		}
	})
	t.Run("empty claim skips the router", func(t *testing.T) {
		router := &fakeReportRouter{handled: always}
		store, c := setup(router)
		store.reports = nil
		batch, err := c.PreparePiggyback(context.Background(), "orch")
		if err != nil || len(batch.Reports) != 0 || len(router.calls) != 0 {
			t.Fatalf("batch=%#v err=%v calls=%d", batch, err, len(router.calls))
		}
	})
}

// The coordinator's run loop and a concurrent piggyback share the router; the
// race detector checks the seam adds no unsynchronized state.
func TestForkReportRouterConcurrentRunAndPiggyback(t *testing.T) {
	now := time.Date(2026, 9, 14, 17, 0, 0, 0, time.UTC)
	base := newCoordinatorStore(now)
	base.sessions = []domain.SessionRecord{{ID: "orch", ProjectID: "p", Kind: domain.KindOrchestrator, Mode: domain.SessionModeChat}}
	router := &fakeReportRouter{handled: always}
	var tokens sync.Mutex
	n := 0
	c := NewCoordinator(CoordinatorDeps{Store: base, Delivery: &coordinatorDelivery{}, Now: func() time.Time { return now }, Router: router, NewToken: func() string {
		tokens.Lock()
		defer tokens.Unlock()
		n++
		return "t" + strings.Repeat("x", n)
	}})
	for i := 0; i < 50; i++ {
		base.mu.Lock()
		base.reports = append(base.reports, reportAt("r"+strings.Repeat("x", i+1), "worker", now, now, "n"))
		base.mu.Unlock()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = c.RunDue(context.Background()) }()
		go func() { defer wg.Done(); _, _ = c.PreparePiggyback(context.Background(), "orch") }()
		wg.Wait()
	}
	base.mu.Lock()
	defer base.mu.Unlock()
	if base.acked != 50 {
		t.Fatalf("acked=%d, want every report handed off exactly once", base.acked)
	}
}

// failingAckStore refuses acknowledgements and releases.
type failingAckStore struct{ *coordinatorStore }

func (failingAckStore) AcknowledgeReportBatch(context.Context, domain.ProjectID, string, time.Time) (int, error) {
	return 0, errors.New("store locked")
}
func (failingAckStore) ReleaseReportBatch(context.Context, domain.ProjectID, string, time.Time, string) (int, error) {
	return 0, errors.New("store locked")
}
