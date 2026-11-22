package report

import (
	"context"
	"fmt"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ReportRouter decides, per claimed batch, whether reports go somewhere other
// than the project's local orchestrator. Fork: fleet seam (FORK.md); the fork
// hands reports of fleet-routed projects to their remote master (PR-D).
//
// Route runs after the batch is claimed and before any local orchestrator
// lookup. It must be idempotent per report: a crash between Route and the
// acknowledgement leaves the batch claimed, the next daemon start requeues it,
// and Route sees the same reports again.
type ReportRouter interface {
	// Route returns handled=true when it took the batch; the coordinator then
	// acknowledges it with no Submit, no Interrupt and no local wake.
	// handled=false keeps today's local delivery. An error releases the batch
	// for a later retry.
	Route(ctx context.Context, projectID domain.ProjectID, batch []domain.ReportRecord) (handled bool, err error)
}

// deliverRouted is deliverProject with a router. It claims first so a routed
// project needs no local orchestrator; an unrouted batch then follows the
// local path, and is released when no orchestrator can take it.
func (c *Coordinator) deliverRouted(ctx context.Context, projectID domain.ProjectID) error {
	batch, err := c.claim(ctx, projectID)
	if err != nil || len(batch.Reports) == 0 {
		return err
	}
	handled, err := c.router.Route(ctx, projectID, slices.Clone(batch.Reports))
	if err != nil {
		return c.release(ctx, batch, fmt.Errorf("route reports: %w", err))
	}
	if handled {
		_, err = c.store.AcknowledgeReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC())
		return err
	}
	target, ok, err := c.activeOrchestrator(ctx, projectID)
	if err != nil {
		return c.release(ctx, batch, err)
	}
	if !ok {
		return c.release(ctx, batch, fmt.Errorf("project %s has no active orchestrator", projectID))
	}
	return c.deliverClaimed(ctx, target, batch)
}

// routePiggyback keeps routed reports out of a local Chat turn. A routed
// batch is acknowledged and nothing is appended. A router failure releases
// the batch for RunDue to retry and appends nothing, so the user's message is
// never refused because of routing; store failures are returned as before.
func (c *Coordinator) routePiggyback(ctx context.Context, batch PreparedBatch) (PreparedBatch, error) {
	if len(batch.Reports) == 0 {
		return batch, nil
	}
	handled, err := c.router.Route(ctx, batch.ProjectID, slices.Clone(batch.Reports))
	if err != nil {
		if _, releaseErr := c.store.ReleaseReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC(), "route reports: "+err.Error()); releaseErr != nil {
			return PreparedBatch{}, releaseErr
		}
		return PreparedBatch{}, nil
	}
	if handled {
		_, err = c.store.AcknowledgeReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC())
		return PreparedBatch{}, err
	}
	return batch, nil
}
