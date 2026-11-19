package usage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// MaxLimitPeakWeeks is as many weeks as the period history keeps.
const MaxLimitPeakWeeks = 26

type limitPeriodStore interface {
	ListAgentLimitPeriods(ctx context.Context, since time.Time) ([]domain.AgentLimitPeriod, error)
	ListAgentLimitDays(ctx context.Context, since time.Time) ([]domain.AgentLimitDay, error)
}

// LimitPeaksReader reads how full each login's limit windows got per reset period.
type LimitPeaksReader struct {
	store limitPeriodStore
	now   func() time.Time
}

// NewLimitPeaksReader constructs a limit peaks reader.
func NewLimitPeaksReader(store limitPeriodStore) *LimitPeaksReader {
	return &LimitPeaksReader{store: store, now: time.Now}
}

type peakTrackKey struct {
	provider domain.AgentAccountProvider
	login    string
	window   string
}

// Peaks reads the periods with a reading in the last weeks, every login AO
// ever read included, so a subscription no longer signed in still shows.
func (r *LimitPeaksReader) Peaks(ctx context.Context, weeks int) (domain.UsageLimitPeaks, error) {
	if r == nil || r.store == nil {
		return domain.UsageLimitPeaks{}, fmt.Errorf("limit period store is unavailable")
	}
	if weeks < 1 || weeks > MaxLimitPeakWeeks {
		return domain.UsageLimitPeaks{}, apierr.Invalid("INVALID_USAGE_WINDOW", fmt.Sprintf("weeks must be between 1 and %d", MaxLimitPeakWeeks), nil)
	}
	until := r.now().UTC()
	out := domain.UsageLimitPeaks{Since: until.AddDate(0, 0, -7*weeks), Until: until, Windows: []domain.UsageLimitPeakTrack{}}
	periods, err := r.store.ListAgentLimitPeriods(ctx, out.Since)
	if err != nil {
		return domain.UsageLimitPeaks{}, err
	}
	days, err := r.store.ListAgentLimitDays(ctx, out.Since)
	if err != nil {
		return domain.UsageLimitPeaks{}, err
	}
	tracks := map[peakTrackKey]*domain.UsageLimitPeakTrack{}
	var order []peakTrackKey
	for _, p := range periods {
		key := peakTrackKey{p.Provider, p.Login, p.WindowID}
		track := tracks[key]
		if track == nil {
			track = &domain.UsageLimitPeakTrack{Provider: p.Provider, Login: p.Login, WindowID: p.WindowID, Days: []domain.UsageLimitDayPeak{}}
			tracks[key] = track
			order = append(order, key)
		}
		// Periods come oldest first: the newest one names the account, plan and label.
		track.Email, track.AccountID, track.Label, track.DurationMinutes = p.Email, p.AccountID, p.Label, p.DurationMinutes
		if p.Plan != "" {
			track.Plan = p.Plan
		}
		track.Periods = append(track.Periods, domain.UsageLimitPeak{
			ResetAt: p.ResetsAt, PeakUsedPct: p.PeakUsedPercent, Readings: p.Readings,
			FirstReadingAt: p.StartedAt, LastReadingAt: p.LastReadAt, Open: p.ResetsAt.After(until),
		})
	}
	for _, d := range days {
		// Days only add to a window that has periods: a window without resets has no history to plan by.
		if track := tracks[peakTrackKey{d.Provider, d.Login, d.WindowID}]; track != nil {
			track.Days = append(track.Days, domain.UsageLimitDayPeak{Day: d.Day, MaxUsedPct: d.MaxUsedPercent, Readings: d.Readings})
		}
	}
	for _, key := range order {
		track := tracks[key]
		track.Summary = domain.SummarizeLimitPeaks(track.Periods)
		out.Windows = append(out.Windows, *track)
	}
	sort.SliceStable(out.Windows, func(i, j int) bool {
		a, b := out.Windows[i], out.Windows[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Login != b.Login {
			return a.Login < b.Login
		}
		return a.WindowID < b.WindowID
	})
	return out, nil
}
