package usage

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeLimitPeriods struct {
	periods []domain.AgentLimitPeriod
	days    []domain.AgentLimitDay
	since   time.Time
}

func (f *fakeLimitPeriods) ListAgentLimitPeriods(_ context.Context, since time.Time) ([]domain.AgentLimitPeriod, error) {
	f.since = since
	return f.periods, nil
}

func (f *fakeLimitPeriods) ListAgentLimitDays(context.Context, time.Time) ([]domain.AgentLimitDay, error) {
	return f.days, nil
}

func TestLimitPeaksGroupsPeriodsAndSummarizesClosedOnes(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	period := func(login, window string, reset time.Time, peak float64) domain.AgentLimitPeriod {
		return domain.AgentLimitPeriod{
			Provider: domain.AgentAccountProviderClaude, Login: login, Email: login, AccountID: "device-claude", Plan: "max",
			WindowID: window, Label: "Weekly", DurationMinutes: 7 * 24 * 60, StartedAt: reset.Add(-week), ResetsAt: reset,
			PeakUsedPercent: peak, Readings: 10, LastReadAt: reset.Add(-time.Hour),
		}
	}
	store := &fakeLimitPeriods{
		periods: []domain.AgentLimitPeriod{
			period("a@example.com", "seven_day", now.Add(-3*week), 85),
			period("a@example.com", "seven_day", now.Add(-2*week), 20),
			period("a@example.com", "seven_day", now.Add(-week), 50),
			period("a@example.com", "seven_day", now.Add(2*24*time.Hour), 95),
			period("a@example.com", "five_hour", now.Add(-time.Hour), 10),
		},
		days: []domain.AgentLimitDay{
			{Provider: domain.AgentAccountProviderClaude, Login: "a@example.com", WindowID: "seven_day", Day: "2026-11-18", MaxUsedPercent: 90, Readings: 60},
			{Provider: domain.AgentAccountProviderClaude, Login: "a@example.com", WindowID: "no_resets", Day: "2026-11-18", MaxUsedPercent: 1, Readings: 1},
		},
	}
	reader := NewLimitPeaksReader(store)
	reader.now = func() time.Time { return now }

	got, err := reader.Peaks(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	if !store.since.Equal(now.AddDate(0, 0, -84)) {
		t.Fatalf("since = %v", store.since)
	}
	if len(got.Windows) != 2 || got.Windows[0].WindowID != "five_hour" || got.Windows[1].WindowID != "seven_day" {
		t.Fatalf("windows = %+v", got.Windows)
	}
	weekly := got.Windows[1]
	if len(weekly.Periods) != 4 || !weekly.Periods[3].Open || weekly.Periods[0].Open || len(weekly.Days) != 1 || weekly.Plan != "max" {
		t.Fatalf("weekly = %+v", weekly)
	}
	want := domain.UsageLimitPeakSummary{Periods: 3, AvgPeak: 155.0 / 3, MaxPeak: 85, PeriodsOver80: 1, PeriodsUnder30: 1}
	if weekly.Summary != want {
		t.Fatalf("summary = %+v, want %+v (the open period left out)", weekly.Summary, want)
	}
}

func TestLimitPeaksRejectsWeeksOutOfRange(t *testing.T) {
	reader := NewLimitPeaksReader(&fakeLimitPeriods{})
	for _, weeks := range []int{0, MaxLimitPeakWeeks + 1} {
		if _, err := reader.Peaks(context.Background(), weeks); err == nil {
			t.Fatalf("weeks %d accepted", weeks)
		}
	}
}
