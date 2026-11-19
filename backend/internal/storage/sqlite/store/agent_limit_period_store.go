package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

const limitDayLayout = "2006-01-02"

// foldAgentLimitReading adds one reading to its login's per-period and per-day aggregates.
// A reading already counted (not newer than the last one folded) changes nothing, so replays are safe.
func foldAgentLimitReading(ctx context.Context, q *gen.Queries, r domain.AgentLimitReading) error {
	login := domain.AgentLimitLogin(r.Email, r.AccountID)
	if err := q.UpsertAgentLimitDay(ctx, gen.UpsertAgentLimitDayParams{
		Provider: string(r.Provider), Login: login, WindowID: r.WindowID, Day: r.ReadAt.UTC().Format(limitDayLayout), Email: r.Email,
		Label: r.Label, DurationMinutes: int64(r.DurationMinutes), MaxUsedPercent: r.UsedPercent, LastReadAtMs: r.ReadAt.UnixMilli(),
	}); err != nil {
		return fmt.Errorf("fold day %s %s: %w", login, r.WindowID, err)
	}
	if r.ResetsAt == nil {
		return nil
	}
	var latest *domain.AgentLimitPeriod
	row, err := q.GetLatestAgentLimitPeriod(ctx, gen.GetLatestAgentLimitPeriodParams{Provider: string(r.Provider), Login: login, WindowID: r.WindowID})
	switch {
	case err == nil:
		period := limitPeriodFromRow(row)
		latest = &period
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read period %s %s: %w", login, r.WindowID, err)
	}
	next, ok := domain.FoldAgentLimitPeriod(latest, r)
	if !ok {
		return nil
	}
	if err := q.UpsertAgentLimitPeriod(ctx, gen.UpsertAgentLimitPeriodParams{
		Provider: string(next.Provider), Login: next.Login, WindowID: next.WindowID, StartedAtMs: next.StartedAt.UnixMilli(), Email: next.Email,
		AccountID: next.AccountID, Plan: next.Plan, Label: next.Label, DurationMinutes: int64(next.DurationMinutes), ResetsAtMs: next.ResetsAt.UnixMilli(),
		PeakUsedPercent: next.PeakUsedPercent, LastUsedPercent: next.LastUsedPercent, Readings: int64(next.Readings), LastReadAtMs: next.LastReadAt.UnixMilli(),
	}); err != nil {
		return fmt.Errorf("fold period %s %s: %w", login, r.WindowID, err)
	}
	return nil
}

func deleteExpiredLimitAggregates(ctx context.Context, q *gen.Queries, now time.Time) error {
	cutoff := now.Add(-domain.LimitPeriodRetention)
	if err := q.DeleteAgentLimitPeriodsBefore(ctx, cutoff.UnixMilli()); err != nil {
		return fmt.Errorf("expire limit periods: %w", err)
	}
	if err := q.DeleteAgentLimitDaysBefore(ctx, cutoff.UTC().Format(limitDayLayout)); err != nil {
		return fmt.Errorf("expire limit days: %w", err)
	}
	return nil
}

// BackfillAgentLimitPeriods folds the kept raw readings into the period and day
// aggregates once, while the aggregates are still empty. It returns how many
// readings it folded.
func (s *Store) BackfillAgentLimitPeriods(ctx context.Context) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	folded := 0
	err := s.inTx(ctx, "backfill agent limit periods", func(q *gen.Queries) error {
		days, err := q.CountAgentLimitDays(ctx)
		if err != nil || days > 0 {
			return err
		}
		rows, err := q.ListAgentLimitReadingsSince(ctx, 0)
		if err != nil {
			return err
		}
		readings := make([]domain.AgentLimitReading, 0, len(rows))
		for _, row := range rows {
			readings = append(readings, limitReadingFromRow(row))
		}
		// The fold expects each login's readings oldest first.
		sortLimitReadingsByTime(readings)
		for _, r := range readings {
			if err := foldAgentLimitReading(ctx, q, r); err != nil {
				return err
			}
		}
		folded = len(readings)
		return nil
	})
	return folded, err
}

// ListAgentLimitPeriods returns the periods with a reading at or after since.
func (s *Store) ListAgentLimitPeriods(ctx context.Context, since time.Time) ([]domain.AgentLimitPeriod, error) {
	rows, err := s.qr.ListAgentLimitPeriodsSince(ctx, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("list agent limit periods: %w", err)
	}
	out := make([]domain.AgentLimitPeriod, 0, len(rows))
	for _, row := range rows {
		out = append(out, limitPeriodFromRow(row))
	}
	return out, nil
}

// ListAgentLimitDays returns the per-day maxima from since's UTC day on.
func (s *Store) ListAgentLimitDays(ctx context.Context, since time.Time) ([]domain.AgentLimitDay, error) {
	rows, err := s.qr.ListAgentLimitDaysSince(ctx, since.UTC().Format(limitDayLayout))
	if err != nil {
		return nil, fmt.Errorf("list agent limit days: %w", err)
	}
	out := make([]domain.AgentLimitDay, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.AgentLimitDay{
			Provider: domain.AgentAccountProvider(row.Provider), Login: row.Login, Email: row.Email, WindowID: row.WindowID, Label: row.Label,
			DurationMinutes: int(row.DurationMinutes), Day: row.Day, MaxUsedPercent: row.MaxUsedPercent, Readings: int(row.Readings),
		})
	}
	return out, nil
}

func limitPeriodFromRow(row gen.AgentLimitPeriod) domain.AgentLimitPeriod {
	return domain.AgentLimitPeriod{
		Provider: domain.AgentAccountProvider(row.Provider), Login: row.Login, Email: row.Email, AccountID: row.AccountID, Plan: row.Plan,
		WindowID: row.WindowID, Label: row.Label, DurationMinutes: int(row.DurationMinutes),
		StartedAt: time.UnixMilli(row.StartedAtMs).UTC(), ResetsAt: time.UnixMilli(row.ResetsAtMs).UTC(),
		PeakUsedPercent: row.PeakUsedPercent, LastUsedPercent: row.LastUsedPercent, Readings: int(row.Readings), LastReadAt: time.UnixMilli(row.LastReadAtMs).UTC(),
	}
}
