package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestAgentLimitPeriodsFoldFromReadings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustNoError(t, s.SetLimitHistory(ctx, true, time.Now()))
	t0 := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	first := t0.Add(2 * 24 * time.Hour)
	second := first.Add(7 * 24 * time.Hour)
	reading := func(at time.Time, used float64, reset time.Time) domain.AgentLimitReading {
		return domain.AgentLimitReading{
			AccountID: "device-claude", Provider: domain.AgentAccountProviderClaude, Email: "a@example.com", Plan: "max",
			WindowID: "seven_day", Label: "Weekly", DurationMinutes: 7 * 24 * 60, UsedPercent: used, ResetsAt: &reset, ReadAt: at,
		}
	}
	for _, r := range []domain.AgentLimitReading{
		reading(t0, 10, first), reading(t0.Add(time.Hour), 60, first), reading(t0.Add(time.Hour), 60, first),
		reading(first.Add(time.Minute), 3, second), reading(first.Add(2*time.Hour), 8, second),
	} {
		mustNoError(t, s.RecordAgentLimitReadings(ctx, []domain.AgentLimitReading{r}))
	}
	periods, err := s.ListAgentLimitPeriods(ctx, t0.Add(-time.Hour))
	mustNoError(t, err)
	if len(periods) != 2 || periods[0].PeakUsedPercent != 60 || periods[0].Readings != 2 || !periods[0].ResetsAt.Equal(first) ||
		periods[1].PeakUsedPercent != 8 || periods[1].Readings != 2 || periods[1].Plan != "max" {
		t.Fatalf("periods = %+v", periods)
	}
	days, err := s.ListAgentLimitDays(ctx, t0)
	mustNoError(t, err)
	if len(days) != 2 || days[0].Day != "2026-11-02" || days[0].MaxUsedPercent != 60 || days[0].Readings != 2 || days[1].MaxUsedPercent != 8 {
		t.Fatalf("days = %+v", days)
	}

	// Raw readings go after 14 days; the aggregates stay for 180.
	late := t0.Add(100 * 24 * time.Hour)
	mustNoError(t, s.RecordAgentLimitReadings(ctx, []domain.AgentLimitReading{reading(late, 1, late.Add(24*time.Hour))}))
	periods, err = s.ListAgentLimitPeriods(ctx, t0.Add(-time.Hour))
	mustNoError(t, err)
	if len(periods) != 3 {
		t.Fatalf("after 100 days periods = %+v, want all three kept", periods)
	}
	mustNoError(t, s.RecordAgentLimitReadings(ctx, []domain.AgentLimitReading{reading(t0.Add(200*24*time.Hour), 1, t0.Add(201*24*time.Hour))}))
	periods, err = s.ListAgentLimitPeriods(ctx, t0.Add(-time.Hour))
	mustNoError(t, err)
	days, err = s.ListAgentLimitDays(ctx, t0)
	mustNoError(t, err)
	if len(periods) != 2 || len(days) != 2 || !periods[0].StartedAt.Equal(late) {
		t.Fatalf("after 200 days periods = %+v days = %+v, want the first weeks expired", periods, days)
	}
}

func TestBackfillAgentLimitPeriodsOnce(t *testing.T) {
	dir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dir)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	reset := t0.Add(3 * time.Hour)
	readings := []domain.AgentLimitReading{
		{AccountID: "acct-b", Provider: domain.AgentAccountProviderCodex, Email: "b@example.com", WindowID: "codex:5h", DurationMinutes: 300, UsedPercent: 40, ResetsAt: &reset, ReadAt: t0.Add(time.Hour)},
		{AccountID: "acct-a", Provider: domain.AgentAccountProviderCodex, Email: "b@example.com", WindowID: "codex:5h", DurationMinutes: 300, UsedPercent: 20, ResetsAt: &reset, ReadAt: t0},
		{AccountID: "acct-a", Provider: domain.AgentAccountProviderCodex, Email: "b@example.com", WindowID: "codex:weekly", UsedPercent: 5, ReadAt: t0},
	}
	mustNoError(t, s.RecordAgentLimitReadings(ctx, readings))
	// The raw readings from before the aggregates existed.
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db")+failureStorePragmas)
	mustNoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.ExecContext(ctx, "DELETE FROM agent_limit_periods; DELETE FROM agent_limit_days"); err != nil {
		t.Fatal(err)
	}

	folded, err := s.BackfillAgentLimitPeriods(ctx)
	mustNoError(t, err)
	if folded != 3 {
		t.Fatalf("folded = %d, want 3", folded)
	}
	periods, err := s.ListAgentLimitPeriods(ctx, t0.Add(-time.Hour))
	mustNoError(t, err)
	// Two accounts signed in as one login share its period; the reading without a reset makes none.
	if len(periods) != 1 || periods[0].Login != "b@example.com" || periods[0].Readings != 2 || periods[0].PeakUsedPercent != 40 || !periods[0].StartedAt.Equal(t0) {
		t.Fatalf("periods = %+v", periods)
	}
	days, err := s.ListAgentLimitDays(ctx, t0)
	mustNoError(t, err)
	if len(days) != 2 {
		t.Fatalf("days = %+v, want the 5h and weekly day", days)
	}

	folded, err = s.BackfillAgentLimitPeriods(ctx)
	mustNoError(t, err)
	periods, _ = s.ListAgentLimitPeriods(ctx, t0.Add(-time.Hour))
	if folded != 0 || periods[0].Readings != 2 {
		t.Fatalf("second backfill folded %d, periods = %+v; want nothing refolded", folded, periods)
	}
}
