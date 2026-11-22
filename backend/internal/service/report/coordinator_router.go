package report

import (
	"context"
	"fmt"
	"slices"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Router decides, per claimed batch, whether reports go somewhere other
// than the project's local orchestrator. Fork: fleet seam (FORK.md); the fork
// hands reports of fleet-routed projects to their remote master (PR-D).
//
// Route runs after the batch is claimed and before any local orchestrator
// lookup. It must be idempotent per report: a crash between Route and the
// acknowledgement leaves the batch claimed, the next daemon start requeues it,
// and Route sees the same reports again. Route may be called concurrently
// (the delivery loop and a Chat piggyback), so it must be safe for that. It
// gets a copy of the batch.
type Router interface {
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
	handled, err := c.router.Route(ctx, projectID, cloneReports(batch.Reports))
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
// the batch for RunDue to retry and appends nothing. The user's message is
// never refused because of routing: a failed release or acknowledgement only
// leaves the batch claimed, which daemon start requeues (RequeueClaimedReports)
// and Route treats idempotently.
func (c *Coordinator) routePiggyback(ctx context.Context, batch PreparedBatch) (PreparedBatch, error) {
	if len(batch.Reports) == 0 {
		return batch, nil
	}
	handled, err := c.router.Route(ctx, batch.ProjectID, cloneReports(batch.Reports))
	switch {
	case err != nil:
		c.releaseForRetry(ctx, batch, err)
	case handled:
		_, _ = c.store.AcknowledgeReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC())
	default:
		return batch, nil
	}
	return PreparedBatch{}, nil
}

// releaseForRetry makes a batch the router refused pending again for RunDue,
// best effort: a failed release leaves it claimed until daemon start.
func (c *Coordinator) releaseForRetry(ctx context.Context, batch PreparedBatch, cause error) {
	_, _ = c.store.ReleaseReportBatch(ctx, batch.ProjectID, batch.Token, c.now().UTC(), "route reports: "+cause.Error())
}

// cloneReports copies the batch, including each report's outputs, so a router
// cannot change what local delivery renders.
func cloneReports(reports []domain.ReportRecord) []domain.ReportRecord {
	out := slices.Clone(reports)
	for i := range out {
		out[i].Outputs = slices.Clone(out[i].Outputs)
	}
	return out
}
