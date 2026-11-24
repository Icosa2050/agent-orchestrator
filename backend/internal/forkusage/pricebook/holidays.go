package pricebook

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// beijing is China Standard Time. China has no DST since 1991, so a fixed
// offset is exact and needs no tzdata.
var beijing = time.FixedZone("CST", 8*60*60)

// HolidayBlock is one holiday period from a State Council notice, inclusive.
type HolidayBlock struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// HolidayYear is one year's notice, transcribed.
type HolidayYear struct {
	Year           int            `json:"year"`
	Notice         string         `json:"notice"`
	SourceURL      string         `json:"sourceUrl"`
	PublishedAt    string         `json:"publishedAt"`
	TranscribedAt  string         `json:"transcribedAt"`
	Holidays       []HolidayBlock `json:"holidays"`
	MakeUpWorkdays []string       `json:"makeUpWorkdays"`
}

type calendarFile struct {
	SchemaVersion int           `json:"schemaVersion"`
	DayBoundary   string        `json:"dayBoundary"`
	Years         []HolidayYear `json:"years"`
}

// Calendar answers holiday questions for Beijing dates.
type Calendar struct {
	years    map[int]HolidayYear
	holidays map[string]string // date -> holiday name
	makeUp   map[string]bool
}

// DayKind classifies one Beijing date.
type DayKind int

// Day kinds.
const (
	DayNormal DayKind = iota
	DayHoliday
	DayMakeUpWorkday
	DayYearMissing
)

// ParseCalendar validates the transcribed notices strictly: every year has at
// least 7 holiday blocks inside the year, no date is both a holiday and a
// make-up workday, and make-up workdays fall on weekends.
func ParseCalendar(data []byte) (*Calendar, error) {
	var f calendarFile
	if err := decodeStrict(data, &f); err != nil {
		return nil, err
	}
	if f.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported schema %d", f.SchemaVersion)
	}
	c := &Calendar{years: map[int]HolidayYear{}, holidays: map[string]string{}, makeUp: map[string]bool{}}
	for _, y := range f.Years {
		if _, dup := c.years[y.Year]; dup {
			return nil, fmt.Errorf("year %d listed twice", y.Year)
		}
		if y.SourceURL == "" || y.PublishedAt == "" {
			return nil, fmt.Errorf("year %d: missing source", y.Year)
		}
		if len(y.Holidays) < 7 {
			return nil, fmt.Errorf("year %d: %d holiday blocks, want at least 7", y.Year, len(y.Holidays))
		}
		for _, h := range y.Holidays {
			from, err := parseDate(h.From, y.Year)
			if err != nil {
				return nil, err
			}
			to, err := parseDate(h.To, y.Year)
			if err != nil {
				return nil, err
			}
			if to.Before(from) || to.Sub(from) > 14*24*time.Hour {
				return nil, fmt.Errorf("year %d: holiday %s has a bad range", y.Year, h.Name)
			}
			for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
				key := d.Format(time.DateOnly)
				if _, dup := c.holidays[key]; dup {
					return nil, fmt.Errorf("year %d: %s in two holiday blocks", y.Year, key)
				}
				c.holidays[key] = h.Name
			}
		}
		for _, s := range y.MakeUpWorkdays {
			d, err := parseDate(s, y.Year)
			if err != nil {
				return nil, err
			}
			if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
				return nil, fmt.Errorf("year %d: make-up workday %s is a weekday", y.Year, s)
			}
			if _, clash := c.holidays[s]; clash {
				return nil, fmt.Errorf("year %d: %s is both a holiday and a make-up workday", y.Year, s)
			}
			c.makeUp[s] = true
		}
		c.years[y.Year] = y
	}
	if len(c.years) == 0 {
		return nil, errors.New("no years")
	}
	return c, nil
}

func parseDate(s string, year int) (time.Time, error) {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad date %q", s)
	}
	// Make-up workdays may precede the year (e.g. a late-December Sunday for
	// New Year); allow the neighbouring years only.
	if d.Year() < year-1 || d.Year() > year+1 {
		return time.Time{}, fmt.Errorf("date %s outside year %d", s, year)
	}
	return d, nil
}

// BeijingDate is the calendar date of t in Beijing.
func BeijingDate(t time.Time) string { return t.In(beijing).Format(time.DateOnly) }

// Day classifies the Beijing date of t.
func (c *Calendar) Day(t time.Time) (DayKind, string) {
	date := BeijingDate(t)
	if _, ok := c.years[t.In(beijing).Year()]; !ok {
		// A date listed by the neighbouring year's notice still counts.
		if name, ok := c.holidays[date]; ok {
			return DayHoliday, name
		}
		if c.makeUp[date] {
			return DayMakeUpWorkday, ""
		}
		return DayYearMissing, ""
	}
	if name, ok := c.holidays[date]; ok {
		return DayHoliday, name
	}
	if c.makeUp[date] {
		return DayMakeUpWorkday, ""
	}
	return DayNormal, ""
}

// HasYear reports whether the notice for year is transcribed.
func (c *Calendar) HasYear(year int) bool {
	_, ok := c.years[year]
	return ok
}

// Years lists the transcribed years with their sources.
func (c *Calendar) Years() []HolidayYear {
	out := make([]HolidayYear, 0, len(c.years))
	for _, hy := range c.years {
		out = append(out, hy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Year < out[j].Year })
	return out
}
