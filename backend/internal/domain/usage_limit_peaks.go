package domain

import "time"

// UsageLimitPeaks is how full each login's limit windows got per reset period,
// from the long-lived history AO keeps of its own limit readings.
type UsageLimitPeaks struct {
	Since   time.Time             `json:"since"`
	Until   time.Time             `json:"until"`
	Windows []UsageLimitPeakTrack `json:"windows"`
}

// UsageLimitPeakTrack is one login's limit window over the asked weeks.
type UsageLimitPeakTrack struct {
	Provider        AgentAccountProvider  `json:"provider" enum:"claude,codex"`
	Login           string                `json:"login"`
	Email           string                `json:"email,omitempty"`
	AccountID       string                `json:"accountId"`
	Plan            string                `json:"plan,omitempty"`
	WindowID        string                `json:"windowId"`
	Label           string                `json:"label"`
	DurationMinutes int                   `json:"durationMinutes,omitempty" minimum:"0"`
	Periods         []UsageLimitPeak      `json:"periods"`
	Days            []UsageLimitDayPeak   `json:"days"`
	Summary         UsageLimitPeakSummary `json:"summary"`
}

// UsageLimitPeak is one reset period: the fullest the window got before resetAt.
type UsageLimitPeak struct {
	ResetAt        time.Time `json:"resetAt"`
	PeakUsedPct    float64   `json:"peakUsedPct" minimum:"0" maximum:"100"`
	Readings       int       `json:"readings" minimum:"1"`
	FirstReadingAt time.Time `json:"firstReadingAt"`
	LastReadingAt  time.Time `json:"lastReadingAt"`
	// Open is true while the period has not reset yet; its peak can still grow.
	Open bool `json:"open"`
}

// UsageLimitDayPeak is the fullest the window got on one UTC day.
type UsageLimitDayPeak struct {
	Day        string  `json:"day" description:"UTC day, YYYY-MM-DD"`
	MaxUsedPct float64 `json:"maxUsedPct" minimum:"0" maximum:"100"`
	Readings   int     `json:"readings" minimum:"1"`
}

// UsageLimitPeakSummary sums up the periods that have reset; the open one is left out.
type UsageLimitPeakSummary struct {
	Periods        int     `json:"periods" minimum:"0"`
	AvgPeak        float64 `json:"avgPeak" minimum:"0" maximum:"100"`
	MaxPeak        float64 `json:"maxPeak" minimum:"0" maximum:"100"`
	PeriodsOver80  int     `json:"periodsOver80" minimum:"0"`
	PeriodsUnder30 int     `json:"periodsUnder30" minimum:"0"`
}

// SummarizeLimitPeaks sums up the closed periods' peaks.
func SummarizeLimitPeaks(periods []UsageLimitPeak) UsageLimitPeakSummary {
	var out UsageLimitPeakSummary
	total := 0.0
	for _, p := range periods {
		if p.Open {
			continue
		}
		out.Periods++
		total += p.PeakUsedPct
		out.MaxPeak = max(out.MaxPeak, p.PeakUsedPct)
		if p.PeakUsedPct >= 80 {
			out.PeriodsOver80++
		}
		if p.PeakUsedPct < 30 {
			out.PeriodsUnder30++
		}
	}
	if out.Periods > 0 {
		out.AvgPeak = total / float64(out.Periods)
	}
	return out
}
