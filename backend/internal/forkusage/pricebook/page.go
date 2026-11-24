package pricebook

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Page is the pricing content parsed from the DeepSeek pricing page.
type Page struct {
	// Currency is "USD" when every price is a plain "$" decimal.
	Currency string
	Models   map[string]PageModel
	Aliases  map[string]string
	Peak     PeakRule
	// StatedHalf is true when the page says off-peak is half of peak.
	StatedHalf bool
}

// PageModel holds one model's peak and off-peak rates from the page.
type PageModel struct {
	Peak, OffPeak Rates
}

// ParseError is a fixed, user-facing reason the page could not be verified.
type ParseError struct{ Reason string }

func (e *ParseError) Error() string { return "unrecognized page layout: " + e.Reason }

func layout(reason string) error { return &ParseError{Reason: reason} }

var (
	footnoteRef   = regexp.MustCompile(`\((\d)\)\s*$`)
	priceCell     = regexp.MustCompile(`^(\$|USD\s*|¥|￥|CNY\s*|RMB\s*)\s*(\d+(?:\.\d+)?)$`)
	clockRange    = regexp.MustCompile(`([0-2][0-9]:[0-5][0-9])\s*[-–—]\s*([0-2][0-9]:[0-5][0-9])`)
	weekdayRange  = regexp.MustCompile(`(?i)(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\s+(?:through|to|-|–)\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)`)
	backticked    = regexp.MustCompile("`([a-z0-9][a-z0-9._-]*)`")
	modelNameCell = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	weekdayOrder  = []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
	weekdayShort  = map[string]string{"monday": "Mon", "tuesday": "Tue", "wednesday": "Wed", "thursday": "Thu", "friday": "Fri", "saturday": "Sat", "sunday": "Sun"}
)

// ParsePage parses the Markdown pricing page. It accepts only an unambiguous
// page: every listed model has three peak and three off-peak plain decimal
// prices in one currency, the pricing footnote yields weekdays, UTC ranges
// and the holiday rule, and the alias footnote parses or is absent.
// Anything else is a *ParseError and nothing is recorded.
func ParsePage(body []byte) (*Page, error) {
	text := string(body)
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "<") || strings.Contains(strings.ToLower(trimmed[:min(len(trimmed), 512)]), "<html") {
		return nil, layout("HTML instead of Markdown")
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	header, rows, err := modelTable(lines)
	if err != nil {
		return nil, err
	}
	// Model columns follow the label columns; their header cell is the
	// model name, optionally with a footnote marker "(n)".
	type column struct {
		index    int
		name     string
		footnote string
	}
	var cols []column
	for i, cell := range header {
		if i == 0 || strings.EqualFold(cell, "MODEL") {
			continue
		}
		name, note := cell, ""
		if m := footnoteRef.FindStringSubmatch(cell); m != nil {
			name, note = strings.TrimSpace(strings.TrimSuffix(cell, m[0])), m[1]
		}
		if !modelNameCell.MatchString(name) {
			return nil, layout("model column header is not a model name")
		}
		cols = append(cols, column{index: i, name: name, footnote: note})
	}
	if len(cols) == 0 {
		return nil, layout("no model columns")
	}
	p := &Page{Models: map[string]PageModel{}, Aliases: map[string]string{}}
	type key struct{ component, window string }
	found := map[string]map[key]string{}
	currencies := map[string]bool{}
	pricingNote := ""
	for _, row := range rows {
		if len(row) != len(header) {
			return nil, layout("table row with a different column count")
		}
		label := strings.ToUpper(row[0])
		if !strings.HasPrefix(label, "PRICING") {
			continue
		}
		if m := footnoteRef.FindStringSubmatch(row[0]); m != nil {
			pricingNote = m[1]
		}
		var component string
		desc := strings.ToUpper(row[1])
		switch {
		case strings.Contains(desc, "CACHE HIT"):
			component = "cacheHit"
		case strings.Contains(desc, "CACHE MISS"):
			component = "cacheMiss"
		case strings.Contains(desc, "OUTPUT"):
			component = "output"
		default:
			return nil, layout("unknown pricing row")
		}
		var window string
		switch strings.ToUpper(strings.TrimSpace(row[2])) {
		case "PEAK":
			window = WindowPeak
		case "OFF-PEAK", "OFF PEAK":
			window = WindowOffPeak
		default:
			return nil, layout("pricing row without a peak/off-peak label")
		}
		for _, col := range cols {
			m := priceCell.FindStringSubmatch(strings.TrimSpace(row[col.index]))
			if m == nil {
				return nil, layout("price cell is not a plain decimal")
			}
			cur := "USD"
			if sym := strings.TrimSpace(m[1]); sym != "$" && sym != "USD" {
				cur = "CNY"
			}
			currencies[cur] = true
			if _, err := parseDecimal(m[2]); err != nil {
				return nil, layout("price cell is not a plain decimal")
			}
			if found[col.name] == nil {
				found[col.name] = map[key]string{}
			}
			k := key{component, window}
			if _, dup := found[col.name][k]; dup {
				return nil, layout("duplicate pricing row")
			}
			found[col.name][k] = m[2]
		}
	}
	if len(currencies) != 1 {
		return nil, layout("prices missing or in mixed currencies")
	}
	for c := range currencies {
		p.Currency = c
	}
	for _, col := range cols {
		f := found[col.name]
		if len(f) != 6 {
			return nil, layout(fmt.Sprintf("model %s lacks a peak or off-peak price", col.name))
		}
		p.Models[col.name] = PageModel{
			Peak:    Rates{CacheHit: f[key{"cacheHit", WindowPeak}], CacheMiss: f[key{"cacheMiss", WindowPeak}], Output: f[key{"output", WindowPeak}]},
			OffPeak: Rates{CacheHit: f[key{"cacheHit", WindowOffPeak}], CacheMiss: f[key{"cacheMiss", WindowOffPeak}], Output: f[key{"output", WindowOffPeak}]},
		}
	}
	if pricingNote == "" {
		return nil, layout("pricing rows without a footnote")
	}
	note := footnote(lines, pricingNote)
	if note == "" {
		return nil, layout("pricing footnote missing")
	}
	if p.Peak, err = parsePeakRule(note); err != nil {
		return nil, err
	}
	p.StatedHalf = strings.Contains(strings.ToLower(note), "half of the peak")
	for _, col := range cols {
		if col.footnote == "" || col.footnote == pricingNote {
			continue
		}
		text := footnote(lines, col.footnote)
		if text == "" {
			return nil, layout("model footnote missing")
		}
		lower := strings.ToLower(text)
		if !strings.Contains(lower, "legacy") {
			continue
		}
		idx := strings.Index(lower, "legacy")
		names := backticked.FindAllStringSubmatch(text[idx:], -1)
		if len(names) == 0 || !strings.Contains(lower, "billed at") {
			return nil, layout("legacy-name footnote not recognized")
		}
		for _, n := range names {
			if n[1] == col.name {
				continue
			}
			if _, isModel := p.Models[n[1]]; isModel {
				return nil, layout("legacy name is also a listed model")
			}
			p.Aliases[n[1]] = col.name
		}
	}
	return p, nil
}

// modelTable finds the "Model Details" table: the first Markdown table whose
// header row starts with a MODEL cell.
func modelTable(lines []string) ([]string, [][]string, error) {
	for i := 0; i < len(lines); i++ {
		cells := splitRow(lines[i])
		if len(cells) < 2 || !strings.EqualFold(cells[0], "MODEL") {
			continue
		}
		if i+1 >= len(lines) || !strings.Contains(lines[i+1], "---") {
			return nil, nil, layout("model table without a separator row")
		}
		var rows [][]string
		for j := i + 2; j < len(lines); j++ {
			row := splitRow(lines[j])
			if row == nil {
				break
			}
			rows = append(rows, row)
		}
		return cells, rows, nil
	}
	return nil, nil, layout("model table not found")
}

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	parts := strings.Split(line[1:len(line)-1], "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// footnote returns the paragraph that starts with "(n)".
func footnote(lines []string, n string) string {
	prefix := "(" + n + ")"
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(l, prefix))
		}
	}
	return ""
}

func parsePeakRule(note string) (PeakRule, error) {
	var rule PeakRule
	if !strings.Contains(note, "UTC") {
		return rule, layout("peak hours not stated in UTC")
	}
	for _, m := range clockRange.FindAllStringSubmatch(note, -1) {
		rule.UTCRanges = append(rule.UTCRanges, [2]string{m[1], m[2]})
	}
	if len(rule.UTCRanges) == 0 {
		return rule, layout("peak hours not found")
	}
	days := weekdayRange.FindStringSubmatch(note)
	if days == nil {
		return rule, layout("peak weekdays not found")
	}
	from, to := indexOf(weekdayOrder, strings.ToLower(days[1])), indexOf(weekdayOrder, strings.ToLower(days[2]))
	if to < from {
		return rule, layout("peak weekday range reversed")
	}
	for i := from; i <= to; i++ {
		rule.Weekdays = append(rule.Weekdays, weekdayShort[weekdayOrder[i]])
	}
	lower := strings.ToLower(note)
	switch {
	case strings.Contains(lower, "excluding chinese public holidays"):
		rule.ExcludeHolidays = "cn"
	case strings.Contains(lower, "holiday"):
		return rule, layout("holiday rule not recognized")
	}
	return rule, nil
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// Outcome of comparing a page with the current version.
const (
	OutcomeUnchanged  = "unchanged"
	OutcomeChanged    = "changed"
	OutcomeSuspicious = "suspicious"
)

// Diff is the difference between the current version and the page.
type Diff struct {
	Outcome      string        `json:"outcome"`
	Currency     string        `json:"currency"`
	Changes      []string      `json:"changes,omitempty"`
	Suspicious   []string      `json:"suspicious,omitempty"`
	NewModels    []string      `json:"newModels,omitempty"`
	Removed      []string      `json:"removedModels,omitempty"`
	Renamed      []string      `json:"renamedModels,omitempty"`
	ModelChanges []ModelChange `json:"modelChanges,omitempty"`
}

// ModelChange is one model's old and new peak rates.
type ModelChange struct {
	Model string `json:"model"`
	From  Rates  `json:"from"`
	To    Rates  `json:"to"`
}

// suspiciousFactor bounds a plausible price move.
var suspiciousFactor = big.NewRat(10, 1)

// Compare diffs the page against current and, on a change, builds the
// candidate version effective at fetchedAt. A suspicious change (a price
// moving more than ×10 either way, a currency other than USD, or more than
// half the current models vanishing) still returns the candidate; the caller
// must not record it without an explicit accept.
func Compare(current Version, page *Page, fetchedAt time.Time, pageSHA string) (Diff, *Version, error) {
	d := Diff{Outcome: OutcomeUnchanged, Currency: page.Currency}
	cur, err := compileVersion(current)
	if err != nil {
		return d, nil, err
	}
	if page.Currency != "USD" {
		d.Suspicious = append(d.Suspicious, "prices are not in USD ($)")
	}
	cand := Version{
		Source: SourcePageAuto, Label: AutoLabel, EffectiveFrom: fetchedAt.UTC(), PageSHA256: pageSHA,
		Evidence:          "effectiveFrom: detection time (true change time unknown); " + SourcePage,
		CheckedAt:         fetchedAt.UTC().Format(time.DateOnly),
		Models:            map[string]ModelPrice{},
		Aliases:           page.Aliases,
		OffPeakMultiplier: current.OffPeakMultiplier,
		Peak:              page.Peak,
	}
	fetched := fetchedAt.UTC()
	cand.FetchedAt = &fetched
	if cand.Aliases == nil {
		cand.Aliases = map[string]string{}
	}
	mult := cur.multiple
	if page.StatedHalf {
		mult = big.NewRat(1, 2)
		cand.OffPeakMultiplier = "0.5"
	}
	for name, pm := range page.Models {
		peak, _ := parseRates(pm.Peak)
		off, _ := parseRates(pm.OffPeak)
		mp := ModelPrice{PeakPerM: pm.Peak}
		for i := range peak {
			if new(big.Rat).Mul(peak[i], mult).Cmp(off[i]) != 0 {
				// The parsed numbers win over the stated ratio.
				o := pm.OffPeak
				mp.OffPeakPerM = &o
				break
			}
		}
		cand.Models[name] = mp
		old, existed := cur.models[name]
		if !existed || old.retired {
			d.NewModels = append(d.NewModels, name)
			continue
		}
		// Both windows: an off-peak-only jump is as suspicious as a peak one.
		if reason := implausibleMove(old.peak, peak); reason != "" {
			d.Suspicious = append(d.Suspicious, fmt.Sprintf("%s peak %s", name, reason))
		} else if reason := implausibleMove(old.offPeak, off); reason != "" {
			d.Suspicious = append(d.Suspicious, fmt.Sprintf("%s off-peak %s", name, reason))
		}
	}
	active := 0
	for name, old := range cur.models {
		if old.retired {
			// A retired model the page lists again carries its new rates; one
			// the page now lists as a legacy alias is billed through it.
			_, back := page.Models[name]
			if _, aliased := page.Aliases[name]; !back && !aliased {
				cand.Models[name] = current.Models[name]
			}
			continue
		}
		active++
		if _, ok := page.Models[name]; !ok {
			// Renamed: the page bills the old name as a legacy alias of
			// another model. The alias wins; earlier events keep the previous
			// version's price for this name.
			if target, aliased := page.Aliases[name]; aliased {
				d.Renamed = append(d.Renamed, name+" now billed as "+target)
				continue
			}
			d.Removed = append(d.Removed, name)
			kept := current.Models[name]
			kept.RetiredOnPage = true
			cand.Models[name] = kept
		}
	}
	if active > 0 && len(d.Removed)*2 > active {
		d.Suspicious = append(d.Suspicious, "more than half of the models vanished from the page")
	}
	// Aliases must point at a priced model; a legacy name the page drops
	// keeps its old target when that model is still kept.
	for alias, target := range current.Aliases {
		if _, ok := cand.Aliases[alias]; ok {
			continue
		}
		if _, listed := page.Models[alias]; listed {
			continue
		}
		if _, ok := cand.Models[target]; ok {
			cand.Aliases[alias] = target
		}
	}
	candC, err := compileVersion(withName(cand, "candidate"))
	if err != nil {
		return d, nil, layout("page content does not form a valid version")
	}
	if candC.sameAsKey == cur.sameAsKey && len(d.Suspicious) == 0 {
		return d, nil, nil
	}
	sort.Strings(d.NewModels)
	sort.Strings(d.Removed)
	sort.Strings(d.Renamed)
	d.Changes = append(describeChanges(cur, candC), prefixed("model ", d.Renamed)...)
	for _, name := range sortedKeys(page.Models) {
		if old, ok := cur.models[name]; ok && !old.retired {
			if ratesOf(old.peak) != cand.Models[name].PeakPerM {
				d.ModelChanges = append(d.ModelChanges, ModelChange{Model: name, From: ratesOf(old.peak), To: cand.Models[name].PeakPerM})
			}
		}
	}
	if len(d.Changes) == 0 && page.Currency != "USD" {
		d.Changes = append(d.Changes, "currency changed to "+page.Currency)
	}
	d.Outcome = OutcomeChanged
	if len(d.Suspicious) > 0 {
		d.Outcome = OutcomeSuspicious
	}
	return d, &cand, nil
}

// implausibleMove reports a component that moved more than 10x either way,
// or to or from zero.
func implausibleMove(old, next [3]*big.Rat) string {
	for i := range old {
		switch {
		case old[i].Sign() > 0 && next[i].Sign() > 0:
			ratio := new(big.Rat).Quo(next[i], old[i])
			if ratio.Cmp(suspiciousFactor) > 0 || new(big.Rat).Mul(ratio, suspiciousFactor).Cmp(big.NewRat(1, 1)) < 0 {
				return "price moved more than 10x"
			}
		case old[i].Sign() != next[i].Sign():
			return "price moved to or from zero"
		}
	}
	return ""
}

func prefixed(prefix string, items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = prefix + s
	}
	return out
}

func withName(v Version, name string) Version {
	v.Version = name
	return v
}

func describeChanges(old, cand compiled) []string {
	var out []string
	if fmt.Sprint(old.weekdays, old.ranges, old.Peak.ExcludeHolidays) != fmt.Sprint(cand.weekdays, cand.ranges, cand.Peak.ExcludeHolidays) {
		out = append(out, "peak hours changed")
	}
	if fmt.Sprint(sortedPairs(old.Aliases)) != fmt.Sprint(sortedPairs(cand.Aliases)) {
		out = append(out, "legacy aliases changed")
	}
	for _, name := range sortedKeys(cand.models) {
		o, ok := old.models[name]
		c := cand.models[name]
		switch {
		case !ok:
			out = append(out, "new model "+name)
		case c.retired && !o.retired:
			out = append(out, "model "+name+" no longer listed (kept, retiredOnPage)")
		case ratesOf(o.peak) != ratesOf(c.peak) || ratesOf(o.offPeak) != ratesOf(c.offPeak):
			out = append(out, "price of "+name+" changed")
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

var (
	errNoPending = errors.New("no pending price change with that page hash")
	errNotUSD    = errors.New("the held change is not priced in USD; the price book supports USD only, so it cannot be accepted")
)
