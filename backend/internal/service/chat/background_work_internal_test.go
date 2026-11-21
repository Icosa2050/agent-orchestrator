package chat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type scriptedBackgroundProbe struct {
	mu      sync.Mutex
	answers []struct{ pending, known bool }
	calls   int
}

func (p *scriptedBackgroundProbe) BackgroundWorkPending(context.Context) (bool, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	answer := p.answers[min(p.calls, len(p.answers)-1)]
	p.calls++
	return answer.pending, answer.known, nil
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func watching(c *Controller) bool {
	c.background.mu.Lock()
	defer c.background.mu.Unlock()
	return c.background.watching
}

// A settled turn whose provider still runs background work is reported as
// waiting until the provider says the work ended.
func TestControllerBackgroundWorkPendingUntilProviderSettles(t *testing.T) {
	previous := backgroundWorkPollInterval
	backgroundWorkPollInterval = time.Millisecond
	t.Cleanup(func() { backgroundWorkPollInterval = previous })

	c := &Controller{harness: domain.HarnessCodex, stopped: make(chan struct{})}
	probe := &scriptedBackgroundProbe{answers: []struct{ pending, known bool }{
		{true, true}, {true, true}, {false, true},
	}}
	c.startBackgroundWorkWatch(probe)
	// Until the provider answers, the session must not read idle.
	if items := c.BackgroundWorkItems(); len(items) != 1 || items[0].Kind != domain.BackgroundWorkProvider {
		t.Fatalf("items before the probe answered = %+v, want provider work", items)
	}
	waitFor(t, func() bool { return !watching(c) })
	if items := c.BackgroundWorkItems(); len(items) != 0 {
		t.Fatalf("items after the work ended = %+v, want none", items)
	}
	if probe.calls != 3 {
		t.Fatalf("probe calls = %d, want 3", probe.calls)
	}
}

// A provider build that cannot answer keeps the previous behavior: idle.
func TestControllerBackgroundWorkUnknownProviderReadsIdle(t *testing.T) {
	c := &Controller{harness: domain.HarnessCodex, stopped: make(chan struct{})}
	c.startBackgroundWorkWatch(&scriptedBackgroundProbe{answers: []struct{ pending, known bool }{{false, false}}})
	waitFor(t, func() bool { return !watching(c) })
	if items := c.BackgroundWorkItems(); len(items) != 0 {
		t.Fatalf("items = %+v, want none for an unanswerable provider", items)
	}
}

func TestControllerBackgroundWorkClearedWhenControllerStops(t *testing.T) {
	previous := backgroundWorkPollInterval
	backgroundWorkPollInterval = time.Hour
	t.Cleanup(func() { backgroundWorkPollInterval = previous })

	c := &Controller{harness: domain.HarnessClaudeCode, stopped: make(chan struct{})}
	probe := &scriptedBackgroundProbe{answers: []struct{ pending, known bool }{{true, true}}}
	c.startBackgroundWorkWatch(probe)
	waitFor(t, func() bool {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		return probe.calls == 1
	})
	close(c.stopped)
	waitFor(t, func() bool { return !watching(c) })
	if items := c.BackgroundWorkItems(); len(items) != 0 {
		t.Fatalf("stopped controller still reports %+v", items)
	}
}

type erroringBackgroundProbe struct {
	mu    sync.Mutex
	calls int
	// answerFirst makes the first call a real pending answer.
	answerFirst bool
}

func (p *erroringBackgroundProbe) BackgroundWorkPending(context.Context) (bool, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.answerFirst && p.calls == 1 {
		return true, true, nil
	}
	return false, false, errors.New("background-terminal response is missing data")
}

func (p *erroringBackgroundProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// A provider that never answers keeps the old behavior: idle, no retry loop.
func TestControllerBackgroundWorkProbeErrorWithoutAnswerReadsIdle(t *testing.T) {
	c := &Controller{harness: domain.HarnessCodex, stopped: make(chan struct{})}
	probe := &erroringBackgroundProbe{}
	c.startBackgroundWorkWatch(probe)
	waitFor(t, func() bool { return !watching(c) })
	if items := c.BackgroundWorkItems(); len(items) != 0 {
		t.Fatalf("items = %+v, want none when the provider never answered", items)
	}
	if probe.count() != 1 {
		t.Fatalf("probe calls = %d, want 1 (no retry loop)", probe.count())
	}
}

// After a real pending answer, transient failures keep it, but a provider that
// stops answering cannot hold the session busy forever.
func TestControllerBackgroundWorkProbeFailuresAfterAnswerAreCapped(t *testing.T) {
	previous := backgroundWorkPollInterval
	backgroundWorkPollInterval = time.Millisecond
	t.Cleanup(func() { backgroundWorkPollInterval = previous })

	c := &Controller{harness: domain.HarnessCodex, stopped: make(chan struct{})}
	probe := &erroringBackgroundProbe{answerFirst: true}
	c.startBackgroundWorkWatch(probe)
	waitFor(t, func() bool { return !watching(c) })
	if items := c.BackgroundWorkItems(); len(items) != 0 {
		t.Fatalf("items = %+v, want none after repeated failures", items)
	}
	if want := 1 + backgroundWorkMaxProbeFailures; probe.count() != want {
		t.Fatalf("probe calls = %d, want %d", probe.count(), want)
	}
}

// A start racing a settling watcher must never leave pending set with no
// watcher left to clear it.
func TestControllerBackgroundWorkStartRacingSettleNeverStrandsPending(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := &Controller{harness: domain.HarnessCodex, stopped: make(chan struct{})}
		probe := &scriptedBackgroundProbe{answers: []struct{ pending, known bool }{{false, true}}}
		var wg sync.WaitGroup
		for j := 0; j < 3; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.startBackgroundWorkWatch(probe)
			}()
		}
		wg.Wait()
		waitFor(t, func() bool { return !watching(c) })
		if items := c.BackgroundWorkItems(); len(items) != 0 {
			t.Fatalf("iteration %d: pending stranded without a watcher: %+v", i, items)
		}
	}
}
