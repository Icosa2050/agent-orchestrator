package domain

import (
	"testing"
	"time"
)

func foldAll(readings ...AgentLimitReading) []AgentLimitPeriod {
	var periods []AgentLimitPeriod
	for _, r := range readings {
		var latest *AgentLimitPeriod
		if len(periods) > 0 {
			latest = &periods[len(periods)-1]
		}
		next, ok := FoldAgentLimitPeriod(latest, r)
		if !ok {
			continue
		}
		if latest != nil && next.StartedAt.Equal(latest.StartedAt) {
			*latest = next
		} else {
			periods = append(periods, next)
		}
	}
	return periods
}

func TestFoldAgentLimitPeriod(t *testing.T) {
	t0 := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	at := func(d time.Duration, used float64, reset time.Time) AgentLimitReading {
		return AgentLimitReading{
			AccountID: "device-claude", Provider: AgentAccountProviderClaude, Email: "A@Example.com", WindowID: "seven_day", Label: "Weekly",
			DurationMinutes: 7 * 24 * 60, UsedPercent: used, ResetsAt: &reset, ReadAt: t0.Add(d),
		}
	}
	first := t0.Add(3 * 24 * time.Hour)
	second := first.Add(week)

	t.Run("reset change starts a period and keeps the peak before it", func(t *testing.T) {
		got := foldAll(
			at(0, 20, first), at(time.Hour, 55, first.Add(30*time.Second)), at(2*time.Hour, 40, first),
			at(3*24*time.Hour+time.Minute, 1, second), at(4*24*time.Hour, 9, second),
		)
		if len(got) != 2 {
			t.Fatalf("periods = %+v, want 2", got)
		}
		if got[0].PeakUsedPercent != 55 || got[0].Readings != 3 || !got[0].ResetsAt.Equal(first) || got[0].Login != "a@example.com" {
			t.Fatalf("first period = %+v", got[0])
		}
		if got[1].PeakUsedPercent != 9 || got[1].Readings != 2 || !got[1].StartedAt.Equal(t0.Add(3*24*time.Hour+time.Minute)) {
			t.Fatalf("second period = %+v", got[1])
		}
	})

	t.Run("missing readings across a reset still split even when the level is higher", func(t *testing.T) {
		got := foldAll(at(0, 30, first), at(5*24*time.Hour, 45, second))
		if len(got) != 2 || got[0].PeakUsedPercent != 30 || got[1].PeakUsedPercent != 45 {
			t.Fatalf("periods = %+v", got)
		}
	})

	t.Run("a reset the provider moves keeps the period", func(t *testing.T) {
		later, earlier := first.Add(26*time.Hour), first.Add(-5*time.Hour)
		got := foldAll(at(0, 30, first), at(time.Hour, 31, later), at(2*time.Hour, 33, earlier), at(3*time.Hour, 34, earlier))
		if len(got) != 1 || got[0].Readings != 4 || got[0].PeakUsedPercent != 34 || !got[0].ResetsAt.Equal(earlier) {
			t.Fatalf("periods = %+v, want one period that took the moved reset", got)
		}
	})

	t.Run("an early reset that drops the level starts a period", func(t *testing.T) {
		got := foldAll(at(0, 70, first), at(time.Hour, 2, second))
		if len(got) != 2 || got[0].PeakUsedPercent != 70 || got[1].PeakUsedPercent != 2 {
			t.Fatalf("periods = %+v", got)
		}
	})

	t.Run("repeated, older and reset-less readings are not counted", func(t *testing.T) {
		noReset := at(3*time.Hour, 99, first)
		noReset.ResetsAt = nil
		got := foldAll(at(time.Hour, 10, first), at(time.Hour, 10, first), at(0, 90, first), noReset)
		if len(got) != 1 || got[0].Readings != 1 || got[0].PeakUsedPercent != 10 {
			t.Fatalf("periods = %+v", got)
		}
	})
}

func TestAgentLimitLogin(t *testing.T) {
	if got := AgentLimitLogin(" A@B.com ", "x"); got != "a@b.com" {
		t.Fatalf("login = %q", got)
	}
	if got := AgentLimitLogin("", "device-codex"); got != "account:device-codex" {
		t.Fatalf("login = %q", got)
	}
}
