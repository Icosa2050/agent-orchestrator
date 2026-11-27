package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Fork (Icosa2050, FORK.md "Usage seams"): the fork DeepSeek event query reads
// upstream columns directly. This test runs it against the migrated schema, so
// an upstream rename of any column it reads fails here.
func TestForkUsageDeepSeekEventsReadsRoutesWindowAndRequestStart(t *testing.T) {
	s := sqlitetest.MustOpen(t)
	ctx := context.Background()
	sess := seedUsageSession(t, s, domain.HarnessCodex)
	now := time.Unix(1700000000, 0).UTC()
	source := seedUsageSource(t, s, sess, now)
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	event := func(key, route, model string, at time.Time, usage string) domain.ModelUsageEvent {
		return domain.ModelUsageEvent{
			ProviderID: domain.UsageProviderOpenAI, BillingProviderID: route, BillingProviderSource: domain.UsageBillingProviderObserved,
			ModelID: model, MeasurementKind: domain.UsageMeasurementNativeReported,
			Tokens: canonicalUsageTokens(30, 10, 20, 5), ProviderUsageJSON: usage, CreatedAt: at, SourceEventKey: key,
		}
	}
	mustNoError(t, s.ApplyUsageChunk(ctx, source.ID, 0, source.UpdatedAt, domain.SourceCursorState{
		ByteOffset: 10, State: domain.UsageSourceActive, UpdatedAt: now,
	}, []domain.ModelUsageEvent{
		event("before", "deepseek-account", "deepseek-v4-pro", day.Add(-time.Second), ""),
		event("spanning", "deepseek-account", "deepseek-v4-pro", day.Add(10*time.Hour+time.Minute),
			`{"inputTokens":20,"requestStartedAt":"2026-11-20T09:58:00.5Z"}`),
		event("legacy", "deepseek", "deepseek-v4-flash", day.Add(11*time.Hour), `{"inputTokens":20}`),
		event("broken", "deepseek", "deepseek-flash", day.Add(12*time.Hour), `{"requestStartedAt":"yesterday"}`),
		event("openai", "openai", "gpt-5", day.Add(13*time.Hour), ""),
		event("after", "deepseek", "deepseek-flash", day.Add(24*time.Hour), ""),
	}))

	events, err := s.ListForkDeepSeekUsageEvents(ctx, day, day.Add(24*time.Hour), []string{"deepseek", "deepseek-account"})
	mustNoError(t, err)
	if len(events) != 3 {
		t.Fatalf("events = %+v, want the three DeepSeek events inside the window", events)
	}
	spanning, legacy, broken := events[0], events[1], events[2]
	wantStart := time.Date(2026, 11, 20, 9, 58, 0, 500e6, time.UTC)
	if spanning.RequestStartedAt == nil || !spanning.RequestStartedAt.Equal(wantStart) {
		t.Fatalf("spanning start = %v, want %v", spanning.RequestStartedAt, wantStart)
	}
	if spanning.BillingProviderID != "deepseek-account" || spanning.ModelID != "deepseek-v4-pro" || spanning.SessionID != sess.ID ||
		spanning.Harness != domain.HarnessCodex || spanning.ProjectID != sess.ProjectID || !spanning.CreatedAt.Equal(day.Add(10*time.Hour+time.Minute)) {
		t.Fatalf("spanning = %+v", spanning)
	}
	if spanning.CachedTokens != 10 || spanning.UncachedTokens != 20 || spanning.OutputTokens != 5 || spanning.CatalogCostNanos != nil {
		t.Fatalf("spanning tokens/cost = %+v", spanning)
	}
	if legacy.RequestStartedAt != nil || broken.RequestStartedAt != nil {
		t.Fatalf("legacy/broken starts = %v/%v, want none", legacy.RequestStartedAt, broken.RequestStartedAt)
	}

	// The snapshot read returns both lists from one read transaction, equal
	// to the separate queries.
	snapDS, snapAll, err := s.ListForkUsageReportSnapshot(ctx, day, day.Add(24*time.Hour), []string{"deepseek", "deepseek-account"})
	mustNoError(t, err)
	all, err := s.ListUsageReportEvents(ctx, day, day.Add(24*time.Hour))
	mustNoError(t, err)
	if !reflect.DeepEqual(snapDS, events) || !reflect.DeepEqual(snapAll, all) || len(snapAll) != 4 {
		t.Fatalf("snapshot differs from the separate reads:\n%+v\n%+v", snapDS, snapAll)
	}

	none, err := s.ListForkDeepSeekUsageEvents(ctx, day, day.Add(24*time.Hour), nil)
	mustNoError(t, err)
	if len(none) != 0 {
		t.Fatalf("events without routes or DeepSeek harness = %+v", none)
	}
}
