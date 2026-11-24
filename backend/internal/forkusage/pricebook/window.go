package pricebook

import (
	"fmt"
	"sort"
	"time"
)

// Window names.
const (
	WindowPeak    = "peak"
	WindowOffPeak = "off-peak"
)

// Reasons an instant is peak or off-peak.
const (
	ReasonPeakHours      = "weekday peak hours"
	ReasonOutsideHours   = "outside peak hours"
	ReasonNonPeakDay     = "weekend"
	ReasonHoliday        = "chinese public holiday"
	ReasonMakeUpWorkday  = "make-up working weekend"
	ReasonNoEvidence     = "before the first evidenced price version"
	AssumptionMakeUpDays = "make-up working weekends are priced off-peak (literal reading: the page says weekends are off-peak in full)"
	AssumptionYearMissed = "holiday calendar for this year is missing: weekday peak hours are priced as peak (upper bound)"
)

// WindowRule describes which instant decides an event's window.
const WindowRule = "reply time; request start for requests spanning a window boundary (assumed; DeepSeek does not specify)"

// segmentHorizon bounds the search for window edges; the longest off-peak
// stretch (a 9-day holiday plus weekends) is far shorter.
const segmentHorizon = 40 * 24 * time.Hour

type state struct {
	peak        bool
	reason      string
	holiday     string
	version     *compiled
	yearMissing bool
	makeUpDay   bool
	noEvidence  bool
}

func (b *Book) stateAt(t time.Time) state {
	t = t.UTC()
	v, ok := b.versionAt(t)
	s := state{}
	if !ok {
		v = &b.versions[0]
		s.noEvidence = true
	}
	s.version = v
	minute := t.Hour()*60 + t.Minute()
	inHours := false
	for _, r := range v.ranges {
		if minute >= r[0] && minute < r[1] {
			inHours = true
			break
		}
	}
	var kind DayKind
	var name string
	if v.Peak.ExcludeHolidays == "cn" {
		kind, name = b.holidays.Day(t)
	}
	switch {
	case !v.weekdays[t.Weekday()]:
		s.reason = ReasonNonPeakDay
		if kind == DayMakeUpWorkday && inHours {
			s.reason, s.makeUpDay = ReasonMakeUpWorkday, true
		}
	case !inHours:
		s.reason = ReasonOutsideHours
	case kind == DayHoliday:
		s.reason, s.holiday = ReasonHoliday, name
	default:
		s.peak, s.reason = true, ReasonPeakHours
		s.yearMissing = kind == DayYearMissing
	}
	if s.noEvidence {
		s.reason = ReasonNoEvidence + "; " + s.reason
	}
	return s
}

// candidates are the instants in [from, to] where the window can change: the
// peak range edges of every version, UTC midnight (weekday), Beijing midnight
// (holiday date) and version effective instants.
func (b *Book) candidates(from, to time.Time) []time.Time {
	minutes := map[int]bool{0: true, 16 * 60: true}
	for _, v := range b.versions {
		for _, r := range v.ranges {
			minutes[r[0]%(24*60)] = true
			minutes[r[1]%(24*60)] = true
		}
	}
	offsets := make([]int, 0, len(minutes))
	for m := range minutes {
		offsets = append(offsets, m)
	}
	sort.Ints(offsets)
	var out []time.Time
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	for ; !day.After(to); day = day.AddDate(0, 0, 1) {
		for _, m := range offsets {
			c := day.Add(time.Duration(m) * time.Minute)
			if !c.Before(from) && !c.After(to) {
				out = append(out, c)
			}
		}
	}
	for _, v := range b.versions {
		if !v.EffectiveFrom.Before(from) && !v.EffectiveFrom.After(to) {
			out = append(out, v.EffectiveFrom.UTC())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// nextChange is the first instant after t whose peak state differs from t's.
func (b *Book) nextChange(t time.Time) (time.Time, bool) {
	cur := b.stateAt(t).peak
	for _, c := range b.candidates(t.Add(time.Nanosecond), t.Add(segmentHorizon)) {
		if b.stateAt(c).peak != cur {
			return c, true
		}
	}
	return time.Time{}, false
}

// segmentStart is the instant the window containing t began.
func (b *Book) segmentStart(t time.Time) (time.Time, bool) {
	cur := b.stateAt(t).peak
	cs := b.candidates(t.Add(-segmentHorizon), t)
	for i := len(cs) - 1; i >= 0; i-- {
		if b.stateAt(cs[i].Add(-time.Nanosecond)).peak != cur {
			return cs[i], true
		}
	}
	return time.Time{}, false
}

// Span is a time range [From, Until).
type Span struct {
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
}

// Window describes the price window at one instant.
type Window struct {
	At     time.Time `json:"at"`
	Window string    `json:"window"`
	// PriceFactor is the price relative to peak; empty off-peak when models
	// have explicit off-peak rates that differ from the multiplier.
	PriceFactor     string     `json:"priceFactor,omitempty"`
	Reason          string     `json:"reason"`
	Holiday         string     `json:"holiday,omitempty"`
	Since           *time.Time `json:"since,omitempty"`
	Until           *time.Time `json:"until,omitempty"`
	NextOffPeak     *Span      `json:"nextOffPeak,omitempty"`
	NextPeak        *Span      `json:"nextPeak,omitempty"`
	Version         string     `json:"version"`
	NoPriceEvidence bool       `json:"noPriceEvidence,omitempty"`
	HolidayCalendar string     `json:"holidayCalendar"`
	Assumptions     []string   `json:"assumptions,omitempty"`
}

// IsPeak reports whether t is in a peak window.
func (b *Book) IsPeak(t time.Time) bool { return b.stateAt(t).peak }

// WindowAt evaluates the window at t, with the current window's edges and
// the next peak and off-peak windows that start after t.
func (b *Book) WindowAt(t time.Time) Window {
	t = t.UTC()
	s := b.stateAt(t)
	w := Window{At: t, Reason: s.reason, Holiday: s.holiday, Version: s.version.Version.Version, NoPriceEvidence: s.noEvidence}
	if s.peak {
		w.Window, w.PriceFactor = WindowPeak, "1"
	} else {
		w.Window = WindowOffPeak
		if s.version.uniformOffPeak() {
			w.PriceFactor = decimalString(s.version.multiple)
		}
	}
	if since, ok := b.segmentStart(t); ok {
		w.Since = &since
	}
	if until, ok := b.nextChange(t); ok {
		w.Until = &until
	}
	w.NextOffPeak = b.nextSpan(t, false)
	w.NextPeak = b.nextSpan(t, true)
	w.HolidayCalendar = b.holidayCalendarNote(t)
	if s.makeUpDay {
		w.Assumptions = append(w.Assumptions, AssumptionMakeUpDays)
	}
	if s.yearMissing {
		w.Assumptions = append(w.Assumptions, AssumptionYearMissed)
	}
	w.Assumptions = append(w.Assumptions, s.version.Assumptions...)
	return w
}

// nextSpan is the first window of the wanted kind that starts after t.
func (b *Book) nextSpan(t time.Time, peak bool) *Span {
	from, ok := b.nextChange(t)
	if !ok {
		return nil
	}
	if b.stateAt(t).peak == peak {
		// t is inside a window of this kind; the next one starts after the
		// opposite window that follows.
		if from, ok = b.nextChange(from); !ok {
			return nil
		}
	}
	until, ok := b.nextChange(from)
	if !ok {
		return nil
	}
	return &Span{From: from, Until: until}
}

// NextStart is the start of the current or next window of the wanted kind:
// t itself when t already is in such a window.
func (b *Book) NextStart(t time.Time, peak bool) (time.Time, bool) {
	t = t.UTC()
	if b.stateAt(t).peak == peak {
		return t, true
	}
	return b.nextChange(t)
}

func (b *Book) holidayCalendarNote(t time.Time) string {
	year := t.In(beijing).Year()
	if b.holidays.HasYear(year) {
		return fmt.Sprintf("%d", year)
	}
	return fmt.Sprintf("%d missing: weekday peak ranges priced as peak (upper bound)", year)
}
