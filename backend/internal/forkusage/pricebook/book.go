// Package pricebook is the fork-owned (Icosa2050, see FORK.md) effective-dated
// DeepSeek price book: embedded, evidenced versions plus versions recorded
// locally by the automatic price-page check, the peak/off-peak window engine
// with the Chinese public holiday calendar, and the read-time pricer.
//
// Nothing here stores costs: callers price token counts at read time, so a
// price-book change applies to every read with no repricing job.
package pricebook

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"
)

//go:embed data/deepseek.json data/cn-holidays.json
var embedded embed.FS

// SourcePage is the public, keyless page the automatic check reads.
const SourcePage = "https://api-docs.deepseek.com/quick_start/pricing.md"

// Version sources.
const (
	SourceEmbedded = "embedded"
	SourcePageAuto = "page"
	// AutoLabel marks a version the price check recorded from the page.
	AutoLabel = "auto-updated from page"
)

// Rates are USD per 1M tokens as decimal strings.
type Rates struct {
	CacheHit  string `json:"cacheHit"`
	CacheMiss string `json:"cacheMiss"`
	Output    string `json:"output"`
}

// ModelPrice is one model's list price in a version.
type ModelPrice struct {
	PeakPerM Rates `json:"peakPerM"`
	// OffPeakPerM is set only when the off-peak price is not
	// offPeakMultiplier × peak.
	OffPeakPerM *Rates `json:"offPeakPerM,omitempty"`
	// RetiredOnPage marks a model the page no longer lists; its last price is
	// kept so history stays priced.
	RetiredOnPage bool `json:"retiredOnPage,omitempty"`
}

// PeakRule says when a version's peak price applies.
type PeakRule struct {
	Weekdays  []string    `json:"weekdays"`
	UTCRanges [][2]string `json:"utcRanges"`
	// ExcludeHolidays is "cn" (Chinese public holidays are off-peak) or "".
	ExcludeHolidays string `json:"excludeHolidays"`
}

// Version is one effective-dated price table.
type Version struct {
	Version           string                `json:"version"`
	EffectiveFrom     time.Time             `json:"effectiveFrom"`
	Source            string                `json:"source"`
	Label             string                `json:"label,omitempty"`
	Evidence          string                `json:"evidence,omitempty"`
	CheckedAt         string                `json:"checkedAt,omitempty"`
	PageSHA256        string                `json:"pageSha256,omitempty"`
	FetchedAt         *time.Time            `json:"fetchedAt,omitempty"`
	Models            map[string]ModelPrice `json:"models"`
	Aliases           map[string]string     `json:"aliases"`
	OffPeakMultiplier string                `json:"offPeakMultiplier"`
	Peak              PeakRule              `json:"peak"`
	Assumptions       []string              `json:"assumptions,omitempty"`
}

// File is the on-disk shape of the embedded book.
type File struct {
	SchemaVersion int               `json:"schemaVersion"`
	Provider      string            `json:"provider"`
	Currency      string            `json:"currency"`
	SourceURL     string            `json:"sourceUrl"`
	Routes        map[string]string `json:"routes"`
	Versions      []Version         `json:"versions"`
}

// Book is a validated, merged price book plus the holiday calendar.
type Book struct {
	Provider string
	Currency string
	Source   string
	routes   map[string]string
	versions []compiled
	holidays *Calendar
}

type compiled struct {
	Version
	weekdays  [7]bool
	ranges    [][2]int // minutes of the UTC day, [start, end)
	multiple  *big.Rat
	models    map[string]compiledModel
	sameAsKey string // canonical pricing content, for de-duplication
}

type compiledModel struct {
	peak, offPeak [3]*big.Rat // cacheHit, cacheMiss, output
	retired       bool
}

var (
	decimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})(\.[0-9]{1,9})?$`)
	clockPattern   = regexp.MustCompile(`^([01][0-9]|2[0-4]):([0-5][0-9])$`)
	weekdayNames   = map[string]time.Weekday{
		"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday,
		"Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday,
	}
)

// decodeStrict decodes one JSON document, rejecting unknown fields and
// trailing data.
func decodeStrict(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON document")
	}
	return nil
}

func parseDecimal(s string) (*big.Rat, error) {
	if !decimalPattern.MatchString(s) {
		return nil, fmt.Errorf("%q is not a plain non-negative decimal", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal", s)
	}
	return r, nil
}

func parseRates(r Rates) ([3]*big.Rat, error) {
	var out [3]*big.Rat
	for i, s := range []string{r.CacheHit, r.CacheMiss, r.Output} {
		v, err := parseDecimal(s)
		if err != nil {
			return out, err
		}
		out[i] = v
	}
	return out, nil
}

func parseClock(s string) (int, error) {
	m := clockPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	minutes := (int(m[1][0]-'0')*10+int(m[1][1]-'0'))*60 + int(m[2][0]-'0')*10 + int(m[2][1]-'0')
	if minutes > 24*60 {
		return 0, fmt.Errorf("%q is past 24:00", s)
	}
	return minutes, nil
}

func compileVersion(v Version) (compiled, error) {
	c := compiled{Version: v}
	if v.Version == "" {
		return c, errors.New("version without a name")
	}
	if v.EffectiveFrom.IsZero() {
		return c, fmt.Errorf("version %s: missing effectiveFrom", v.Version)
	}
	if v.Source != SourceEmbedded && v.Source != SourcePageAuto {
		return c, fmt.Errorf("version %s: unknown source %q", v.Version, v.Source)
	}
	mult, err := parseDecimal(v.OffPeakMultiplier)
	if err != nil {
		return c, fmt.Errorf("version %s: offPeakMultiplier: %w", v.Version, err)
	}
	if mult.Cmp(new(big.Rat).SetInt64(1)) > 0 {
		return c, fmt.Errorf("version %s: offPeakMultiplier above 1", v.Version)
	}
	c.multiple = mult
	if len(v.Peak.Weekdays) == 0 && len(v.Peak.UTCRanges) > 0 {
		return c, fmt.Errorf("version %s: peak ranges without weekdays", v.Version)
	}
	for _, name := range v.Peak.Weekdays {
		day, ok := weekdayNames[name]
		if !ok {
			return c, fmt.Errorf("version %s: unknown weekday %q", v.Version, name)
		}
		if c.weekdays[day] {
			return c, fmt.Errorf("version %s: weekday %s listed twice", v.Version, name)
		}
		c.weekdays[day] = true
	}
	for _, pair := range v.Peak.UTCRanges {
		start, err := parseClock(pair[0])
		if err != nil {
			return c, fmt.Errorf("version %s: %w", v.Version, err)
		}
		end, err := parseClock(pair[1])
		if err != nil {
			return c, fmt.Errorf("version %s: %w", v.Version, err)
		}
		if end <= start {
			return c, fmt.Errorf("version %s: empty or reversed range %s-%s", v.Version, pair[0], pair[1])
		}
		c.ranges = append(c.ranges, [2]int{start, end})
	}
	sort.Slice(c.ranges, func(i, j int) bool { return c.ranges[i][0] < c.ranges[j][0] })
	for i := 1; i < len(c.ranges); i++ {
		if c.ranges[i][0] < c.ranges[i-1][1] {
			return c, fmt.Errorf("version %s: overlapping peak ranges", v.Version)
		}
	}
	switch v.Peak.ExcludeHolidays {
	case "", "cn":
	default:
		return c, fmt.Errorf("version %s: unknown holiday calendar %q", v.Version, v.Peak.ExcludeHolidays)
	}
	if len(v.Models) == 0 {
		return c, fmt.Errorf("version %s: no models", v.Version)
	}
	c.models = make(map[string]compiledModel, len(v.Models))
	for name, price := range v.Models {
		peak, err := parseRates(price.PeakPerM)
		if err != nil {
			return c, fmt.Errorf("version %s model %s: %w", v.Version, name, err)
		}
		m := compiledModel{peak: peak, retired: price.RetiredOnPage}
		if price.OffPeakPerM != nil {
			if m.offPeak, err = parseRates(*price.OffPeakPerM); err != nil {
				return c, fmt.Errorf("version %s model %s off-peak: %w", v.Version, name, err)
			}
		} else {
			for i := range peak {
				m.offPeak[i] = new(big.Rat).Mul(peak[i], mult)
			}
		}
		c.models[name] = m
	}
	for alias, target := range v.Aliases {
		if _, ok := c.models[target]; !ok {
			return c, fmt.Errorf("version %s: alias %s points at unknown model %s", v.Version, alias, target)
		}
		if _, ok := c.models[alias]; ok {
			return c, fmt.Errorf("version %s: alias %s shadows a model", v.Version, alias)
		}
	}
	c.sameAsKey = contentKey(c)
	return c, nil
}

// contentKey is the pricing content of a version: models (effective peak and
// off-peak rates), aliases and the peak rule. Two versions with equal keys
// price every event identically.
func contentKey(c compiled) string {
	type model struct {
		Peak, OffPeak [3]string
		Retired       bool
	}
	models := map[string]model{}
	for name, m := range c.models {
		var mm model
		for i := 0; i < 3; i++ {
			mm.Peak[i] = m.peak[i].RatString()
			mm.OffPeak[i] = m.offPeak[i].RatString()
		}
		mm.Retired = m.retired
		models[name] = mm
	}
	data, _ := json.Marshal(struct {
		Models  map[string]model
		Aliases map[string]string
		Days    [7]bool
		Ranges  [][2]int
		Holiday string
	}{models, c.Aliases, c.weekdays, c.ranges, c.Peak.ExcludeHolidays})
	return string(data)
}

// LoadEmbedded returns the book built into the binary.
func LoadEmbedded() (*Book, error) {
	return Load(nil)
}

// Load merges the embedded versions with locally recorded auto versions.
func Load(auto []Version) (*Book, error) {
	data, err := embedded.ReadFile("data/deepseek.json")
	if err != nil {
		return nil, err
	}
	hdata, err := embedded.ReadFile("data/cn-holidays.json")
	if err != nil {
		return nil, err
	}
	cal, err := ParseCalendar(hdata)
	if err != nil {
		return nil, fmt.Errorf("holiday calendar: %w", err)
	}
	return build(data, auto, cal)
}

func build(data []byte, auto []Version, cal *Calendar) (*Book, error) {
	var file File
	if err := decodeStrict(data, &file); err != nil {
		return nil, fmt.Errorf("price book: %w", err)
	}
	if file.SchemaVersion != 1 || file.Provider != "deepseek" || file.Currency != "USD" {
		return nil, fmt.Errorf("price book: unsupported schema %d / provider %q / currency %q", file.SchemaVersion, file.Provider, file.Currency)
	}
	for route, billing := range file.Routes {
		if billing != "api" {
			return nil, fmt.Errorf("price book: route %s has unknown billing %q", route, billing)
		}
	}
	b := &Book{Provider: file.Provider, Currency: file.Currency, Source: file.SourceURL, routes: file.Routes, holidays: cal}
	seen := map[string]bool{}
	var embeddedVersions []compiled
	for _, v := range file.Versions {
		if v.Source != SourceEmbedded {
			return nil, fmt.Errorf("price book: embedded version %s has source %q", v.Version, v.Source)
		}
		c, err := compileVersion(v)
		if err != nil {
			return nil, fmt.Errorf("price book: %w", err)
		}
		if seen[c.Version.Version] {
			return nil, fmt.Errorf("price book: duplicate version %s", c.Version.Version)
		}
		seen[c.Version.Version] = true
		embeddedVersions = append(embeddedVersions, c)
	}
	if len(embeddedVersions) == 0 {
		return nil, errors.New("price book: no versions")
	}
	all := append([]compiled(nil), embeddedVersions...)
	for _, v := range auto {
		if v.Source != SourcePageAuto {
			return nil, fmt.Errorf("auto version %s has source %q", v.Version, v.Source)
		}
		c, err := compileVersion(v)
		if err != nil {
			return nil, fmt.Errorf("auto version: %w", err)
		}
		if seen[c.Version.Version] {
			return nil, fmt.Errorf("duplicate version %s", c.Version.Version)
		}
		seen[c.Version.Version] = true
		all = append(all, c)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].EffectiveFrom.Before(all[j].EffectiveFrom) })
	// Embedded versions win: an auto version that repeats the pricing of the
	// version already in effect before it adds nothing and is dropped, so a
	// shipped version with the same numbers takes precedence.
	merged := all[:0:0]
	for _, c := range all {
		if c.Source == SourcePageAuto && len(merged) > 0 && merged[len(merged)-1].sameAsKey == c.sameAsKey {
			continue
		}
		if len(merged) > 0 && merged[len(merged)-1].EffectiveFrom.Equal(c.EffectiveFrom) {
			if c.Source == SourcePageAuto {
				continue
			}
			merged = merged[:len(merged)-1]
		}
		merged = append(merged, c)
	}
	b.versions = merged
	return b, nil
}

// Versions lists the merged versions, oldest first.
func (b *Book) Versions() []Version {
	out := make([]Version, len(b.versions))
	for i, c := range b.versions {
		out[i] = c.Version
	}
	return out
}

// Latest is the newest version (the one in effect now and in the future).
func (b *Book) Latest() Version { return b.versions[len(b.versions)-1].Version }

// Calendar is the holiday calendar the book uses.
func (b *Book) Calendar() *Calendar { return b.holidays }

// versionAt returns the version whose [effectiveFrom, next) contains t.
func (b *Book) versionAt(t time.Time) (*compiled, bool) {
	i := sort.Search(len(b.versions), func(i int) bool { return b.versions[i].EffectiveFrom.After(t) })
	if i == 0 {
		return nil, false
	}
	return &b.versions[i-1], true
}

// VersionAt returns the version in effect at t; false before the first
// evidenced version.
func (b *Book) VersionAt(t time.Time) (Version, bool) {
	c, ok := b.versionAt(t)
	if !ok {
		return Version{}, false
	}
	return c.Version, true
}

// RouteBilling maps a usage billing route to its billing kind ("api"), or "".
func (b *Book) RouteBilling(route string) string { return b.routes[route] }
