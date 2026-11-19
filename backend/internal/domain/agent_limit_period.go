package domain

import (
	"strings"
	"time"
)

// LimitPeriodRetention is how long the per-period and per-day limit aggregates are kept.
const LimitPeriodRetention = 180 * 24 * time.Hour

const (
	// limitSameResetWithin treats reset times this close as the same reset: providers round them.
	limitSameResetWithin = 2 * time.Minute
	// limitDropTolerance is how far a window's level may fall without counting as a reset.
	limitDropTolerance = 1.0
)

// AgentLimitLogin names the login a limit belongs to: the lower-cased email,
// or the account when the provider reported none. Limits are per login, so two
// accounts signed in as the same email share it.
func AgentLimitLogin(email, accountID string) string {
	if who := strings.ToLower(strings.TrimSpace(email)); who != "" {
		return who
	}
	return "account:" + accountID
}

// AgentLimitPeriod is one reset period of one login's limit window: how full
// the window got before it reset.
type AgentLimitPeriod struct {
	Provider        AgentAccountProvider
	Login           string
	Email           string
	AccountID       string
	Plan            string
	WindowID        string
	Label           string
	DurationMinutes int
	StartedAt       time.Time
	ResetsAt        time.Time
	PeakUsedPercent float64
	LastUsedPercent float64
	Readings        int
	LastReadAt      time.Time
}

// AgentLimitDay is the fullest one login's limit window got on one UTC day.
type AgentLimitDay struct {
	Provider        AgentAccountProvider
	Login           string
	Email           string
	WindowID        string
	Label           string
	DurationMinutes int
	Day             string
	MaxUsedPercent  float64
	Readings        int
}

// FoldAgentLimitPeriod adds a reading to the latest known period of its login
// and window, or starts the next period. It returns false for a reading
// without a reset time or one not newer than the period's last reading, which
// is already counted.
//
// A reading continues the period when its reset is the same, give or take the
// provider's rounding. A different reset starts a new period once the old
// reset has passed, or when the level dropped (an early reset). Otherwise the
// provider moved the reset: the period continues and takes the new time.
func FoldAgentLimitPeriod(latest *AgentLimitPeriod, r AgentLimitReading) (AgentLimitPeriod, bool) {
	if r.ResetsAt == nil {
		return AgentLimitPeriod{}, false
	}
	reset := r.ResetsAt.UTC()
	if latest != nil && !r.ReadAt.After(latest.LastReadAt) {
		return AgentLimitPeriod{}, false
	}
	if latest == nil || !continuesLimitPeriod(*latest, r.ReadAt, reset, r.UsedPercent) {
		return AgentLimitPeriod{
			Provider: r.Provider, Login: AgentLimitLogin(r.Email, r.AccountID), Email: r.Email, AccountID: r.AccountID, Plan: r.Plan,
			WindowID: r.WindowID, Label: r.Label, DurationMinutes: r.DurationMinutes, StartedAt: r.ReadAt.UTC(), ResetsAt: reset,
			PeakUsedPercent: r.UsedPercent, LastUsedPercent: r.UsedPercent, Readings: 1, LastReadAt: r.ReadAt.UTC(),
		}, true
	}
	next := *latest
	next.ResetsAt = reset
	next.PeakUsedPercent = max(next.PeakUsedPercent, r.UsedPercent)
	next.LastUsedPercent = r.UsedPercent
	next.Readings++
	next.LastReadAt = r.ReadAt.UTC()
	next.AccountID = r.AccountID
	if r.Email != "" {
		next.Email = r.Email
	}
	if r.Plan != "" {
		next.Plan = r.Plan
	}
	if r.Label != "" {
		next.Label = r.Label
	}
	if r.DurationMinutes > 0 {
		next.DurationMinutes = r.DurationMinutes
	}
	return next, true
}

func continuesLimitPeriod(p AgentLimitPeriod, readAt, reset time.Time, used float64) bool {
	if reset.Sub(p.ResetsAt).Abs() <= limitSameResetWithin {
		return true
	}
	if !readAt.Before(p.ResetsAt.Add(limitSameResetWithin)) {
		return false
	}
	return used >= p.LastUsedPercent-limitDropTolerance
}
