package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Fork (Icosa2050, FORK.md "Usage seams"): a read-only query over upstream
// usage tables for the fork DeepSeek report extension. It uses the same joins
// and window as ListUsageReportEvents and adds the request start the DeepSeek
// parser records in provider_usage_json. TestForkUsageDeepSeekEvents* fails
// if upstream renames a column it reads.

// ForkDeepSeekUsageEvent is one DeepSeek reply as the fork report reads it.
type ForkDeepSeekUsageEvent struct {
	CreatedAt         time.Time
	ProviderID        domain.UsageProviderID
	BillingProviderID string
	ModelID           string
	ServiceTier       string
	CachedTokens      int64
	UncachedTokens    int64
	OutputTokens      int64
	// CatalogCostNanos is upstream's stored estimate (peak-only catalog), nil
	// when the event is unpriced there.
	CatalogCostNanos *int64
	// RequestStartedAt is the start of the request that produced the reply,
	// nil for rows ingested before the parser recorded it.
	RequestStartedAt *time.Time
	Harness          domain.AgentHarness
	SessionID        domain.SessionID
	ProjectID        domain.ProjectID
}

const forkListDeepSeekUsageEvents = `
SELECT
    model_usage_events.created_at,
    model_usage_events.provider_id,
    COALESCE(model_usage_events.billing_provider_id, ''),
    model_usage_events.model_id,
    COALESCE(model_usage_events.service_tier, ''),
    COALESCE(model_usage_events.cached_input_tokens, 0),
    COALESCE(model_usage_events.uncached_input_tokens, 0),
    COALESCE(model_usage_events.output_tokens, 0),
    model_usage_events.estimated_cost_nanos,
    CAST(COALESCE(CASE WHEN json_valid(model_usage_events.provider_usage_json)
        THEN json_extract(model_usage_events.provider_usage_json, '$.requestStartedAt') END, '') AS TEXT),
    usage_bindings.harness,
    usage_bindings.session_id,
    COALESCE(sessions.project_id, '')
FROM model_usage_events
JOIN usage_bindings ON usage_bindings.id = model_usage_events.binding_id
JOIN sessions ON sessions.id = usage_bindings.session_id
WHERE model_usage_events.created_at IS NOT NULL
  AND substr(model_usage_events.created_at, 1, 19) >= CAST(?1 AS TEXT)
  AND substr(model_usage_events.created_at, 1, 19) < CAST(?2 AS TEXT)
  AND (usage_bindings.harness = ?3
       OR model_usage_events.billing_provider_id IN (SELECT value FROM json_each(?4)))
ORDER BY model_usage_events.id
`

// ListForkDeepSeekUsageEvents reads the DeepSeek events in [since, until):
// every event of the DeepSeek Harness, plus any event billed to one of the
// given routes (the price book's route table).
func (s *Store) ListForkDeepSeekUsageEvents(ctx context.Context, since, until time.Time, routes []string) ([]ForkDeepSeekUsageEvent, error) {
	return listForkDeepSeekUsageEvents(ctx, s.readDB, since, until, routes)
}

// ListForkUsageReportSnapshot reads the DeepSeek events and the upstream
// report events (ListUsageReportEvents) of [since, until) in one read-only
// transaction, so both describe the same database snapshot: an event ingested
// meanwhile is in both lists or in neither.
func (s *Store) ListForkUsageReportSnapshot(ctx context.Context, since, until time.Time, routes []string) ([]ForkDeepSeekUsageEvent, []domain.UsageReportEvent, error) {
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	deepseek, err := listForkDeepSeekUsageEvents(ctx, tx, since, until, routes)
	if err != nil {
		return nil, nil, err
	}
	all, err := s.listUsageReportEvents(ctx, s.qr.WithTx(tx), since, until)
	if err != nil {
		return nil, nil, err
	}
	return deepseek, all, nil
}

type forkQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listForkDeepSeekUsageEvents(ctx context.Context, db forkQuerier, since, until time.Time, routes []string) ([]ForkDeepSeekUsageEvent, error) {
	const layout = "2006-01-02 15:04:05"
	if routes == nil {
		routes = []string{}
	}
	routeJSON, err := json.Marshal(routes)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, forkListDeepSeekUsageEvents,
		since.UTC().Format(layout), until.UTC().Format(layout), string(domain.HarnessDeepSeek), string(routeJSON))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ForkDeepSeekUsageEvent{}
	for rows.Next() {
		var (
			e         ForkDeepSeekUsageEvent
			createdAt sql.NullTime
			cost      sql.NullInt64
			started   string
			provider  string
			harness   string
			session   string
			project   string
		)
		if err := rows.Scan(&createdAt, &provider, &e.BillingProviderID, &e.ModelID, &e.ServiceTier,
			&e.CachedTokens, &e.UncachedTokens, &e.OutputTokens, &cost, &started, &harness, &session, &project); err != nil {
			return nil, err
		}
		e.CreatedAt = createdAt.Time.UTC()
		e.ProviderID, e.Harness = domain.UsageProviderID(provider), domain.AgentHarness(harness)
		e.SessionID, e.ProjectID = domain.SessionID(session), domain.ProjectID(project)
		e.CatalogCostNanos = nullInt64Ptr(cost)
		// A malformed start counts as absent: the reply time decides.
		if t, err := time.Parse(time.RFC3339Nano, started); started != "" && err == nil {
			t = t.UTC()
			e.RequestStartedAt = &t
		}
		out = append(out, e)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}
