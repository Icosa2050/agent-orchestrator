// Package reportext is the fork-owned (Icosa2050, see FORK.md "Usage seams")
// DeepSeek extension of the usage report. It prices every DeepSeek event at
// read time with the effective-dated price book (peak/off-peak windows,
// Chinese public holidays, legacy model aliases, route billing) and returns
// rows that join the upstream report rows by (period, harness, model, speed),
// per-session DeepSeek costs and models, and a 24-bucket UTC hour histogram.
// Nothing is stored; a price-book change applies to the next read.
package reportext

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
	"github.com/aoagents/agent-orchestrator/backend/internal/pricing"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Schema is the report-ext JSON schema version.
const Schema = 1

// Window bases: which instant decided an event's peak/off-peak window.
const (
	BasisReplyTime           = "reply-time"
	BasisRequestStart        = "request-start"
	BasisReplyTimeUnverified = "reply-time-unverified"
)

// BoundarySlack is how long after a window edge a reply without a recorded
// request start may have started in the other window.
const BoundarySlack = 5 * time.Minute

// BoundaryRule explains boundaryTokens.
const BoundaryRule = "replies without a recorded request start that land within 5 min after a window edge are priced by reply time and counted in boundaryTokens (their window is unverified)"

// Cost bases of a session model.
const (
	CostBasisPricebook = "pricebook"
	CostBasisCatalog   = "catalog"
	CostBasisNone      = "none"
)

const (
	dayLayout  = "2006-01-02"
	hourLayout = "2006-01-02T15:00"
)

// Window is the report window, as the upstream report computes it.
type Window struct {
	Days       int
	TimeZone   string
	Resolution string // "day" or "hour"
	Since      time.Time
	Until      time.Time
	Periods    []string
}

// Input is everything one report-ext build reads.
type Input struct {
	Window Window
	Book   *pricebook.Book
	// DeepSeek are the fork query's DeepSeek events in the window.
	DeepSeek []store.ForkDeepSeekUsageEvent
	// All are the upstream report events in the window, for sessionModels.
	All       []domain.UsageReportEvent
	ProjectID domain.ProjectID
}

// Tokens are DeepSeek token counts: cache hit, cache miss, output.
type Tokens struct {
	CacheHit  int64 `json:"cacheHitTokens"`
	CacheMiss int64 `json:"cacheMissTokens"`
	Output    int64 `json:"outputTokens"`
}

func (t *Tokens) add(o Tokens) {
	t.CacheHit += o.CacheHit
	t.CacheMiss += o.CacheMiss
	t.Output += o.Output
}

// Total is the sum of the three counts.
func (t Tokens) Total() int64 { return t.CacheHit + t.CacheMiss + t.Output }

// Bucket is the priced events of one window kind.
type Bucket struct {
	Events int64 `json:"events"`
	Tokens
	CostNanos int64 `json:"costNanos"`
}

// WindowBasis counts events by the instant that decided their window.
type WindowBasis struct {
	ReplyTime           int64 `json:"reply-time"`
	RequestStart        int64 `json:"request-start"`
	ReplyTimeUnverified int64 `json:"reply-time-unverified"`
}

func (w *WindowBasis) add(o WindowBasis) {
	w.ReplyTime += o.ReplyTime
	w.RequestStart += o.RequestStart
	w.ReplyTimeUnverified += o.ReplyTimeUnverified
}

// Costs are the read-time and stored cost sums of a set of events. Pointers
// are nil when no event of the set is priced that way.
type Costs struct {
	PricedEvents int64 `json:"pricedEvents"`
	// PricebookCostNanos is the windowed read-time price of the priced events.
	PricebookCostNanos *int64 `json:"pricebookCostNanos"`
	// PeakListCostNanos prices the same events at peak list rates, the
	// comparison a peak-only table gives.
	PeakListCostNanos *int64 `json:"peakListCostNanos"`
	// CatalogCostNanos is upstream's stored estimate (peak-only LiteLLM
	// catalog) over CatalogPricedEvents.
	CatalogCostNanos    *int64 `json:"catalogCostNanos"`
	CatalogPricedEvents int64  `json:"catalogPricedEvents"`
}

func addPtr(p **int64, v int64) {
	if *p == nil {
		*p = new(int64)
	}
	**p += v
}

func (c *Costs) add(o Costs) {
	c.PricedEvents += o.PricedEvents
	c.CatalogPricedEvents += o.CatalogPricedEvents
	for _, pair := range [][2]**int64{{&c.PricebookCostNanos, &o.PricebookCostNanos}, {&c.PeakListCostNanos, &o.PeakListCostNanos}, {&c.CatalogCostNanos, &o.CatalogCostNanos}} {
		if *pair[1] != nil {
			addPtr(pair[0], **pair[1])
		}
	}
}

// PricedRow is the DeepSeek events of one upstream row (period, harness,
// model, speed) that share a route, price version and billing status.
type PricedRow struct {
	Period  string `json:"period"`
	Harness string `json:"harness"`
	// Model is the raw model id as stored (the upstream row key); legacy names
	// stay visible here and BilledAs names the price-book model.
	Model        string `json:"model"`
	ModelMissing bool   `json:"modelMissing,omitempty"`
	Speed        string `json:"speed"`
	Route        string `json:"route"`
	// RouteBilling is how the price book bills the route ("api"), or "".
	RouteBilling  string `json:"routeBilling"`
	BilledAs      string `json:"billedAs,omitempty"`
	BillingStatus string `json:"billingStatus"`
	PriceVersion  string `json:"priceVersion,omitempty"`
	Events        int64  `json:"events"`
	Tokens
	Peak           Bucket      `json:"peak"`
	OffPeak        Bucket      `json:"offPeak"`
	BoundaryTokens int64       `json:"boundaryTokens"`
	WindowBasis    WindowBasis `json:"windowBasis"`
	Costs
	// HolidayYearMissingEvents were priced peak on a weekday whose holiday
	// calendar is not transcribed (an upper bound).
	HolidayYearMissingEvents int64 `json:"holidayYearMissingEvents,omitempty"`
}

// SessionRow is the DeepSeek events of one upstream session row (session,
// harness, model, speed).
type SessionRow struct {
	SessionID       string           `json:"sessionId"`
	ProjectID       string           `json:"projectId"`
	Harness         string           `json:"harness"`
	Model           string           `json:"model"`
	ModelMissing    bool             `json:"modelMissing,omitempty"`
	Speed           string           `json:"speed"`
	BilledAs        string           `json:"billedAs,omitempty"`
	Events          int64            `json:"events"`
	BillingStatuses map[string]int64 `json:"billingStatuses"`
	Tokens
	BoundaryTokens int64 `json:"boundaryTokens"`
	Costs
}

// SessionModel is one model a session used, for the MODELS view.
type SessionModel struct {
	Model        string `json:"model"`
	ModelMissing bool   `json:"modelMissing,omitempty"`
	Harness      string `json:"harness"`
	BilledAs     string `json:"billedAs,omitempty"`
	Events       int64  `json:"events"`
	Tokens       int64  `json:"tokens"`
	// CostNanos is the price-book cost for DeepSeek models and upstream's
	// catalog estimate otherwise; CostBasis says which (or "none").
	CostNanos *int64 `json:"costNanos"`
	CostBasis string `json:"costBasis"`
}

// Hour is one UTC hour of the day over the whole window.
type Hour struct {
	Hour int    `json:"hour"`
	UTC  string `json:"utc"`
	// PeakHour is true when the hour lies in the weekday peak ranges; events
	// on weekends and Chinese public holidays in it are still off-peak.
	PeakHour bool  `json:"peakHour"`
	Events   int64 `json:"events"`
	Tokens
	Peak    Bucket `json:"peak"`
	OffPeak Bucket `json:"offPeak"`
	Costs
}

// PeakRule is the peak rule of the price version in effect at the window end.
type PeakRule struct {
	Version         string      `json:"version"`
	Weekdays        []string    `json:"weekdays"`
	UTCRanges       [][2]string `json:"utcRanges"`
	ExcludeHolidays string      `json:"excludeHolidays"`
}

// Totals sum every DeepSeek event in the window.
type Totals struct {
	Events int64 `json:"events"`
	Tokens
	Peak            Bucket           `json:"peak"`
	OffPeak         Bucket           `json:"offPeak"`
	BoundaryTokens  int64            `json:"boundaryTokens"`
	WindowBasis     WindowBasis      `json:"windowBasis"`
	BillingStatuses map[string]int64 `json:"billingStatuses"`
	Costs
}

// Report is the report-ext answer.
type Report struct {
	Schema          int                       `json:"schema"`
	Status          string                    `json:"status"`
	Provider        string                    `json:"provider"`
	Currency        string                    `json:"currency"`
	Days            int                       `json:"days"`
	TimeZone        string                    `json:"timeZone"`
	Resolution      string                    `json:"resolution"`
	Since           time.Time                 `json:"since"`
	Until           time.Time                 `json:"until"`
	Periods         []string                  `json:"periods"`
	WindowsTimeZone string                    `json:"windowsTimeZone"`
	WindowRule      string                    `json:"windowRule"`
	BoundaryRule    string                    `json:"boundaryRule"`
	PeakRule        *PeakRule                 `json:"peakRule,omitempty"`
	PriceVersions   []string                  `json:"priceVersions"`
	PricedRows      []PricedRow               `json:"pricedRows"`
	SessionRows     []SessionRow              `json:"sessionRows"`
	SessionModels   map[string][]SessionModel `json:"sessionModels"`
	Hours           []Hour                    `json:"hours"`
	Totals          Totals                    `json:"totals"`
	PriceBook       *BookRef                  `json:"priceBook,omitempty"`
	Warnings        []string                  `json:"warnings"`
}

// BookRef is the price-book state behind a report.
type BookRef struct {
	Version         string     `json:"version"`
	Source          string     `json:"source"`
	Label           string     `json:"label,omitempty"`
	EffectiveFrom   time.Time  `json:"effectiveFrom"`
	LastCheckAt     *time.Time `json:"lastCheckAt,omitempty"`
	LastCheckStatus string     `json:"lastCheckStatus,omitempty"`
	Stale           bool       `json:"stale"`
	Warnings        []string   `json:"warnings"`
}

// classified is one DeepSeek event with its window instant and price.
type classified struct {
	event      store.ForkDeepSeekUsageEvent
	at         time.Time
	tokens     Tokens
	basis      string
	boundary   bool
	priced     pricebook.Priced
	peakList   pricebook.Priced
	speed      string
	routeBills string
}

// windowInstant applies the window rule: the reply time decides, except
// that a request whose start lies in another window than its reply is priced
// by its start. A reply without a recorded start shortly after an edge is
// priced by reply time and flagged unverified.
func windowInstant(book *pricebook.Book, reply time.Time, start *time.Time) (time.Time, string, bool) {
	if start != nil && !start.After(reply) {
		if book.IsPeak(*start) != book.IsPeak(reply) {
			return *start, BasisRequestStart, false
		}
		return reply, BasisReplyTime, false
	}
	if book.IsPeak(reply.Add(-BoundarySlack)) != book.IsPeak(reply) {
		return reply, BasisReplyTimeUnverified, true
	}
	return reply, BasisReplyTime, false
}

func classify(book *pricebook.Book, e store.ForkDeepSeekUsageEvent) (classified, error) {
	c := classified{
		event:      e,
		tokens:     Tokens{CacheHit: max(e.CachedTokens, 0), CacheMiss: max(e.UncachedTokens, 0), Output: max(e.OutputTokens, 0)},
		speed:      pricing.SpeedTier(e.ProviderID, e.ServiceTier),
		routeBills: book.RouteBilling(e.BillingProviderID),
	}
	c.at, c.basis, c.boundary = windowInstant(book, e.CreatedAt, e.RequestStartedAt)
	u := pricebook.Usage{Route: e.BillingProviderID, Model: e.ModelID, At: c.at,
		CacheHitTokens: c.tokens.CacheHit, CacheMissTokens: c.tokens.CacheMiss, OutputTokens: c.tokens.Output}
	var err error
	if c.priced, err = book.Price(u); err != nil {
		return c, err
	}
	if c.priced.Status == pricebook.StatusPriced {
		if c.peakList, err = book.PriceAtPeak(u); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (c classified) costs() Costs {
	var out Costs
	if c.priced.Status == pricebook.StatusPriced {
		out.PricedEvents = 1
		addPtr(&out.PricebookCostNanos, c.priced.TotalNanos)
		addPtr(&out.PeakListCostNanos, c.peakList.TotalNanos)
	}
	if c.event.CatalogCostNanos != nil {
		out.CatalogPricedEvents = 1
		addPtr(&out.CatalogCostNanos, *c.event.CatalogCostNanos)
	}
	return out
}

func (c classified) basisCount() WindowBasis {
	switch c.basis {
	case BasisRequestStart:
		return WindowBasis{RequestStart: 1}
	case BasisReplyTimeUnverified:
		return WindowBasis{ReplyTimeUnverified: 1}
	default:
		return WindowBasis{ReplyTime: 1}
	}
}

// addWindow adds a priced event to the peak or off-peak bucket.
func (c classified) addWindow(peak, offPeak *Bucket) {
	if c.priced.Status != pricebook.StatusPriced {
		return
	}
	b := offPeak
	if c.priced.Window == pricebook.WindowPeak {
		b = peak
	}
	b.Events++
	b.add(c.tokens)
	b.CostNanos += c.priced.TotalNanos
}

func (c classified) boundaryTokens() int64 {
	if c.boundary {
		return c.tokens.Total()
	}
	return 0
}

// Build prices the window's DeepSeek events and assembles the report.
func Build(in Input) (Report, error) {
	loc, err := time.LoadLocation(in.Window.TimeZone)
	if err != nil {
		return Report{}, fmt.Errorf("report time zone %q: %w", in.Window.TimeZone, err)
	}
	periodLayout := dayLayout
	if in.Window.Resolution == "hour" {
		periodLayout = hourLayout
	}
	rep := Report{
		Schema: Schema, Status: "ok", Provider: in.Book.Provider, Currency: in.Book.Currency,
		Days: in.Window.Days, TimeZone: in.Window.TimeZone, Resolution: in.Window.Resolution,
		Since: in.Window.Since, Until: in.Window.Until, Periods: in.Window.Periods,
		WindowsTimeZone: "UTC", WindowRule: pricebook.WindowRule, BoundaryRule: BoundaryRule,
		PriceVersions: []string{}, PricedRows: []PricedRow{}, SessionRows: []SessionRow{},
		SessionModels: map[string][]SessionModel{}, Warnings: []string{},
		Totals: Totals{BillingStatuses: map[string]int64{}},
	}
	if rep.Periods == nil {
		rep.Periods = []string{}
	}
	if v, ok := in.Book.VersionAt(in.Window.Until.Add(-time.Nanosecond)); ok {
		rep.PeakRule = &PeakRule{Version: v.Version, Weekdays: v.Peak.Weekdays, UTCRanges: v.Peak.UTCRanges, ExcludeHolidays: v.Peak.ExcludeHolidays}
	}
	rep.Hours = newHours(rep.PeakRule)

	type rowKey struct{ period, harness, model, speed, route, version, status string }
	rows := map[rowKey]*PricedRow{}
	type sessionKey struct{ session, harness, model, speed string }
	sessions := map[sessionKey]*SessionRow{}
	versions := map[string]bool{}
	holidayMissing := false

	for _, e := range in.DeepSeek {
		if in.ProjectID != "" && e.ProjectID != in.ProjectID {
			continue
		}
		c, err := classify(in.Book, e)
		if err != nil {
			return Report{}, fmt.Errorf("price deepseek event at %s: %w", e.CreatedAt.Format(time.RFC3339), err)
		}
		costs, basis := c.costs(), c.basisCount()
		status := c.priced.Status
		if c.priced.Version != "" {
			versions[c.priced.Version] = true
		}
		if c.priced.HolidayYearMissing {
			holidayMissing = true
		}

		key := rowKey{period: e.CreatedAt.In(loc).Format(periodLayout), harness: string(e.Harness), model: e.ModelID,
			speed: c.speed, route: e.BillingProviderID, version: c.priced.Version, status: status}
		row := rows[key]
		if row == nil {
			row = &PricedRow{Period: key.period, Harness: key.harness, Model: key.model, ModelMissing: strings.TrimSpace(key.model) == "",
				Speed: key.speed, Route: key.route, RouteBilling: c.routeBills, BilledAs: c.priced.BilledAs,
				BillingStatus: status, PriceVersion: key.version}
			rows[key] = row
		}
		row.Events++
		row.Tokens.add(c.tokens)
		c.addWindow(&row.Peak, &row.OffPeak)
		row.BoundaryTokens += c.boundaryTokens()
		row.WindowBasis.add(basis)
		row.Costs.add(costs)
		if c.priced.HolidayYearMissing {
			row.HolidayYearMissingEvents++
		}

		sk := sessionKey{session: string(e.SessionID), harness: string(e.Harness), model: e.ModelID, speed: c.speed}
		srow := sessions[sk]
		if srow == nil {
			srow = &SessionRow{SessionID: sk.session, ProjectID: string(e.ProjectID), Harness: sk.harness, Model: sk.model,
				ModelMissing: strings.TrimSpace(sk.model) == "", Speed: sk.speed, BillingStatuses: map[string]int64{}}
			sessions[sk] = srow
		}
		if srow.BilledAs == "" {
			srow.BilledAs = c.priced.BilledAs
		}
		srow.Events++
		srow.BillingStatuses[status]++
		srow.Tokens.add(c.tokens)
		srow.BoundaryTokens += c.boundaryTokens()
		srow.Costs.add(costs)

		h := &rep.Hours[c.windowHour()]
		h.Events++
		h.Tokens.add(c.tokens)
		c.addWindow(&h.Peak, &h.OffPeak)
		h.Costs.add(costs)

		t := &rep.Totals
		t.Events++
		t.Tokens.add(c.tokens)
		c.addWindow(&t.Peak, &t.OffPeak)
		t.BoundaryTokens += c.boundaryTokens()
		t.WindowBasis.add(basis)
		t.BillingStatuses[status]++
		t.Costs.add(costs)
	}

	for _, row := range rows {
		rep.PricedRows = append(rep.PricedRows, *row)
	}
	sort.Slice(rep.PricedRows, func(i, j int) bool {
		a, b := rep.PricedRows[i], rep.PricedRows[j]
		return lessStrings([]string{a.Period, a.Harness, a.Model, a.Speed, a.Route, a.PriceVersion, a.BillingStatus},
			[]string{b.Period, b.Harness, b.Model, b.Speed, b.Route, b.PriceVersion, b.BillingStatus})
	})
	for _, row := range sessions {
		rep.SessionRows = append(rep.SessionRows, *row)
	}
	sort.Slice(rep.SessionRows, func(i, j int) bool {
		a, b := rep.SessionRows[i], rep.SessionRows[j]
		if ca, cb := deref(a.PricebookCostNanos), deref(b.PricebookCostNanos); ca != cb {
			return ca > cb
		}
		return lessStrings([]string{a.SessionID, a.Harness, a.Model, a.Speed}, []string{b.SessionID, b.Harness, b.Model, b.Speed})
	})
	for v := range versions {
		rep.PriceVersions = append(rep.PriceVersions, v)
	}
	sort.Strings(rep.PriceVersions)
	rep.SessionModels = sessionModels(in, rep.SessionRows)

	if n := rep.Totals.BillingStatuses[pricebook.StatusNoPriceEvidence]; n > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d DeepSeek events predate the first evidenced price version: tokens shown, cost unknown", n))
	}
	if n := rep.Totals.BillingStatuses[pricebook.StatusUnknownModel]; n > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d DeepSeek events have a model the price book does not know: tokens shown, cost unknown", n))
	}
	if n := rep.Totals.BillingStatuses[pricebook.StatusUnknownRoute]; n > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d DeepSeek events use a route the price book does not bill: tokens shown, cost unknown", n))
	}
	if holidayMissing {
		rep.Warnings = append(rep.Warnings, pricebook.AssumptionYearMissed)
	}
	return rep, nil
}

// windowHour is the UTC hour of the instant that decided the window.
func (c classified) windowHour() int { return c.at.UTC().Hour() }

// newHours builds the 24 UTC buckets with their peak-hour marks.
func newHours(rule *PeakRule) []Hour {
	hours := make([]Hour, 24)
	var ranges [][2]int
	if rule != nil && len(rule.Weekdays) > 0 {
		for _, r := range rule.UTCRanges {
			from, errFrom := clockMinutes(r[0])
			until, errUntil := clockMinutes(r[1])
			if errFrom == nil && errUntil == nil {
				ranges = append(ranges, [2]int{from, until})
			}
		}
	}
	for h := range hours {
		hours[h] = Hour{Hour: h, UTC: fmt.Sprintf("%02d:00", h)}
		for _, r := range ranges {
			if h*60 < r[1] && (h+1)*60 > r[0] {
				hours[h].PeakHour = true
			}
		}
	}
	return hours
}

func clockMinutes(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		if s == "24:00" {
			return 24 * 60, nil
		}
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// sessionModels lists each session's models, most tokens first. DeepSeek
// models carry the price-book cost; others upstream's catalog estimate.
func sessionModels(in Input, deepseek []SessionRow) map[string][]SessionModel {
	type key struct{ session, harness, model string }
	ds := map[key]SessionRow{}
	for _, r := range deepseek {
		k := key{r.SessionID, r.Harness, r.Model}
		prev, ok := ds[k]
		if !ok {
			ds[k] = r
			continue
		}
		// Speed tiers of one model merge into one MODELS entry.
		prev.Events += r.Events
		prev.Costs.add(r.Costs)
		if prev.BilledAs == "" {
			prev.BilledAs = r.BilledAs
		}
		ds[k] = prev
	}
	models := map[key]*SessionModel{}
	for _, e := range in.All {
		if in.ProjectID != "" && e.ProjectID != in.ProjectID {
			continue
		}
		k := key{string(e.SessionID), string(e.Harness), e.ModelID}
		m := models[k]
		if m == nil {
			m = &SessionModel{Model: e.ModelID, ModelMissing: strings.TrimSpace(e.ModelID) == "", Harness: k.harness, CostBasis: CostBasisNone}
			models[k] = m
		}
		m.Events++
		m.Tokens += max(deref(e.Tokens.CachedInputTokens), 0) + max(deref(e.Tokens.UncachedInputTokens), 0) + max(deref(e.Tokens.OutputTokens), 0)
		if _, isDeepSeek := ds[k]; !isDeepSeek && e.Costs.EstimatedCostNanos != nil {
			addPtr(&m.CostNanos, *e.Costs.EstimatedCostNanos)
			m.CostBasis = CostBasisCatalog
		}
	}
	for k, r := range ds {
		m := models[k]
		if m == nil {
			// The DeepSeek query and the upstream query read the same window;
			// an event only one of them saw is still listed.
			m = &SessionModel{Model: r.Model, ModelMissing: r.ModelMissing, Harness: r.Harness, Events: r.Events, Tokens: r.Total()}
			models[k] = m
		}
		m.BilledAs = r.BilledAs
		m.CostNanos, m.CostBasis = nil, CostBasisNone
		if r.PricebookCostNanos != nil {
			v := *r.PricebookCostNanos
			m.CostNanos, m.CostBasis = &v, CostBasisPricebook
		}
	}
	out := map[string][]SessionModel{}
	for k, m := range models {
		out[k.session] = append(out[k.session], *m)
	}
	for id := range out {
		list := out[id]
		sort.Slice(list, func(i, j int) bool {
			if list[i].Tokens != list[j].Tokens {
				return list[i].Tokens > list[j].Tokens
			}
			return lessStrings([]string{list[i].Model, list[i].Harness}, []string{list[j].Model, list[j].Harness})
		})
	}
	return out
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func lessStrings(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
