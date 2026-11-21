package chat

import (
	"context"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// backgroundWorkPollInterval paces re-checks while a settled session still has
// provider background work. Claude starts its own cycle when a task finishes,
// but a Codex background terminal can end without any provider event.
var backgroundWorkPollInterval = 20 * time.Second

// backgroundWorkProbeTimeout bounds one provider inventory request.
const backgroundWorkProbeTimeout = 5 * time.Second

// backgroundWorkState is the controller's in-memory view of provider work that
// outlived its last settled turn. It never changes persisted activity: the
// session read model reports it, so a session whose turn ended while that work
// runs reads as busy rather than idle, and delivery still treats it as idle.
type backgroundWorkState struct {
	mu       sync.Mutex
	pending  bool
	watching bool
}

// watchBackgroundWork starts (at most one) watcher after a turn settled idle.
// Until the provider answers, the session is reported as still waiting: an
// unanswered question must not read as idle. A provider build that cannot
// answer (known=false) keeps the previous behavior and reads idle.
func (c *Controller) watchBackgroundWork() {
	if probe, ok := c.conv.(ports.ChatBackgroundWorkProbe); ok {
		c.startBackgroundWorkWatch(probe)
	}
}

func (c *Controller) startBackgroundWorkWatch(probe ports.ChatBackgroundWorkProbe) {
	c.background.mu.Lock()
	c.background.pending = true
	if c.background.watching {
		c.background.mu.Unlock()
		return
	}
	c.background.watching = true
	c.background.mu.Unlock()
	go c.runBackgroundWorkWatcher(probe)
}

func (c *Controller) runBackgroundWorkWatcher(probe ports.ChatBackgroundWorkProbe) {
	defer func() {
		c.background.mu.Lock()
		c.background.watching = false
		c.background.mu.Unlock()
	}()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), backgroundWorkProbeTimeout)
		pending, known, err := probe.BackgroundWorkPending(ctx)
		cancel()
		c.background.mu.Lock()
		if err == nil {
			c.background.pending = known && pending
		}
		// A failed probe keeps the previous answer and retries.
		settled := err == nil && !c.background.pending
		c.background.mu.Unlock()
		if settled {
			return
		}
		if err != nil && c.log != nil {
			c.log.Debug("chat background-work probe failed", "session", c.sessionID, "error", err)
		}
		select {
		case <-c.stopped:
			c.clearBackgroundWork()
			return
		case <-time.After(backgroundWorkPollInterval):
		}
	}
}

func (c *Controller) clearBackgroundWork() {
	c.background.mu.Lock()
	c.background.pending = false
	c.background.mu.Unlock()
}

// BackgroundWorkItems reports provider work this controller saw outlive its
// last settled turn.
func (c *Controller) BackgroundWorkItems() []domain.BackgroundWorkItem {
	select {
	case <-c.stopped:
		return nil
	default:
	}
	c.background.mu.Lock()
	defer c.background.mu.Unlock()
	if !c.background.pending {
		return nil
	}
	return []domain.BackgroundWorkItem{{Kind: domain.BackgroundWorkProvider, Type: string(c.harness)}}
}

// BackgroundWork reports a live session controller's pending provider work.
// It reads in-memory controller state only and never contacts the provider.
func (s *Service) BackgroundWork(id domain.SessionID) []domain.BackgroundWorkItem {
	controller, err := s.Controller(id)
	if err != nil || controller == nil {
		return nil
	}
	return controller.BackgroundWorkItems()
}
