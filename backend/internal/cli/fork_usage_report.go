package cli

// Fork (Icosa2050, FORK.md "Usage seams"): `ao usage report` merges the
// daemon's read-time DeepSeek report extension (GET
// /api/v1/usage/fork/report-ext) into the upstream report, and
// `ao usage report --by hour` prints the DeepSeek hour-of-day histogram.
// usage.go calls in through `Fork: usage seam (FORK.md)` markers only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

const forkReportExtPath = "usage/fork/report-ext"

// forkReportExtParams keeps the window parameters report-ext shares with the
// upstream report (days, offset, tz, projectId).
func forkReportExtParams(params url.Values) url.Values {
	out := url.Values{}
	for _, name := range []string{"days", "offset", "tz", "projectId"} {
		if v := params.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	return out
}

type forkCosts struct {
	PricedEvents        int64  `json:"pricedEvents"`
	PricebookCostNanos  *int64 `json:"pricebookCostNanos"`
	PeakListCostNanos   *int64 `json:"peakListCostNanos"`
	CatalogCostNanos    *int64 `json:"catalogCostNanos"`
	CatalogPricedEvents int64  `json:"catalogPricedEvents"`
	// ReplacedCatalogCostNanos is the catalog estimate of the events the
	// price book priced (the part of costNanos pricebookCostNanos replaces).
	ReplacedCatalogCostNanos int64 `json:"replacedCatalogCostNanos"`
}

func (c *forkCosts) add(o forkCosts) {
	c.PricedEvents += o.PricedEvents
	c.CatalogPricedEvents += o.CatalogPricedEvents
	c.ReplacedCatalogCostNanos += o.ReplacedCatalogCostNanos
	for _, pair := range [][2]**int64{{&c.PricebookCostNanos, &o.PricebookCostNanos}, {&c.PeakListCostNanos, &o.PeakListCostNanos}, {&c.CatalogCostNanos, &o.CatalogCostNanos}} {
		if *pair[1] != nil {
			if *pair[0] == nil {
				*pair[0] = new(int64)
			}
			**pair[0] += **pair[1]
		}
	}
}

type forkWindowBasis struct {
	ReplyTime           int64 `json:"reply-time"`
	RequestStart        int64 `json:"request-start"`
	ReplyTimeUnverified int64 `json:"reply-time-unverified"`
}

type forkTokens struct {
	CacheHit  int64 `json:"cacheHitTokens"`
	CacheMiss int64 `json:"cacheMissTokens"`
	Output    int64 `json:"outputTokens"`
}

func (t forkTokens) total() int64 { return t.CacheHit + t.CacheMiss + t.Output }

type forkBucket struct {
	Events int64 `json:"events"`
	forkTokens
	CostNanos int64 `json:"costNanos"`
}

type forkPricedRow struct {
	Period         string          `json:"period"`
	Harness        string          `json:"harness"`
	Model          string          `json:"model"`
	Speed          string          `json:"speed"`
	Route          string          `json:"route"`
	BilledAs       string          `json:"billedAs"`
	BillingStatus  string          `json:"billingStatus"`
	PriceVersion   string          `json:"priceVersion"`
	Events         int64           `json:"events"`
	BoundaryTokens int64           `json:"boundaryTokens"`
	WindowBasis    forkWindowBasis `json:"windowBasis"`
	forkCosts
}

type forkSessionRow struct {
	SessionID       string           `json:"sessionId"`
	Harness         string           `json:"harness"`
	Model           string           `json:"model"`
	Speed           string           `json:"speed"`
	BilledAs        string           `json:"billedAs"`
	Events          int64            `json:"events"`
	BillingStatuses map[string]int64 `json:"billingStatuses"`
	BoundaryTokens  int64            `json:"boundaryTokens"`
	forkCosts
}

type forkHour struct {
	Hour     int    `json:"hour"`
	UTC      string `json:"utc"`
	PeakHour bool   `json:"peakHour"`
	Events   int64  `json:"events"`
	forkTokens
	Peak    forkBucket `json:"peak"`
	OffPeak forkBucket `json:"offPeak"`
	forkCosts
}

type forkPeakRule struct {
	Version         string      `json:"version"`
	Weekdays        []string    `json:"weekdays"`
	UTCRanges       [][2]string `json:"utcRanges"`
	ExcludeHolidays string      `json:"excludeHolidays"`
}

type forkReportTotals struct {
	Events int64 `json:"events"`
	forkTokens
	Peak            forkBucket       `json:"peak"`
	OffPeak         forkBucket       `json:"offPeak"`
	BoundaryTokens  int64            `json:"boundaryTokens"`
	WindowBasis     forkWindowBasis  `json:"windowBasis"`
	BillingStatuses map[string]int64 `json:"billingStatuses"`
	forkCosts
}

type forkBookRef struct {
	Version         string     `json:"version"`
	Source          string     `json:"source"`
	Label           string     `json:"label,omitempty"`
	LastCheckAt     *time.Time `json:"lastCheckAt,omitempty"`
	LastCheckStatus string     `json:"lastCheckStatus,omitempty"`
	Stale           bool       `json:"stale"`
	Warnings        []string   `json:"warnings"`
}

// forkReportExt is the part of the report-ext answer the CLI reads; the
// merged JSON carries the daemon's answer as is.
type forkReportExt struct {
	Schema        int              `json:"schema"`
	Resolution    string           `json:"resolution"`
	Days          int              `json:"days"`
	Since         time.Time        `json:"since"`
	Until         time.Time        `json:"until"`
	WindowRule    string           `json:"windowRule"`
	BoundaryRule  string           `json:"boundaryRule"`
	PeakRule      *forkPeakRule    `json:"peakRule"`
	PriceVersions []string         `json:"priceVersions"`
	PricedRows    []forkPricedRow  `json:"pricedRows"`
	SessionRows   []forkSessionRow `json:"sessionRows"`
	Hours         []forkHour       `json:"hours"`
	Totals        forkReportTotals `json:"totals"`
	PriceBook     *forkBookRef     `json:"priceBook"`
	Warnings      []string         `json:"warnings"`
}

// forkRowExt is what a merged upstream row gains under its "ext" key.
type forkRowExt struct {
	BilledAs        string           `json:"billedAs,omitempty"`
	Routes          []string         `json:"routes"`
	PriceVersions   []string         `json:"priceVersions"`
	BillingStatuses map[string]int64 `json:"billingStatuses"`
	Events          int64            `json:"events"`
	BoundaryTokens  int64            `json:"boundaryTokens"`
	WindowBasis     *forkWindowBasis `json:"windowBasis,omitempty"`
	forkCosts
	// EffectiveCostNanos is the row's costNanos with the catalog estimate of
	// the price-book-priced events replaced by their price-book cost; the
	// row's rank. Nil when the price book priced none of its events.
	EffectiveCostNanos *int64 `json:"effectiveCostNanos"`
	routes, versions   map[string]bool
}

func (x *forkRowExt) note(billedAs, route, version, status string, events, boundary int64, costs forkCosts) {
	if x.routes == nil {
		x.routes, x.versions, x.BillingStatuses = map[string]bool{}, map[string]bool{}, map[string]int64{}
	}
	if x.BilledAs == "" {
		x.BilledAs = billedAs
	}
	if route != "" {
		x.routes[route] = true
	}
	if version != "" {
		x.versions[version] = true
	}
	x.BillingStatuses[status] += events
	x.Events += events
	x.BoundaryTokens += boundary
	x.add(costs)
}

func (x *forkRowExt) finish() {
	x.Routes, x.PriceVersions = sortedSet(x.routes), sortedSet(x.versions)
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func forkRowKey(parts ...string) string { return strings.Join(parts, "\x00") }

// forkFetchReportExt reads report-ext; the status is "ok", "unsupported"
// (the daemon predates it) or "error".
func forkFetchReportExt(ctx context.Context, c *commandContext, params url.Values) (json.RawMessage, string, error) {
	var raw json.RawMessage
	if err := c.getJSON(ctx, apiPath(forkReportExtPath, forkReportExtParams(params)), &raw); err != nil {
		var apiErr apiResponseError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, "unsupported", err
		}
		return nil, "error", err
	}
	return raw, "ok", nil
}

// forkMergeUsageReport merges report-ext into one host's upstream report,
// both read from that host. The upstream keys stay as the daemon sent them;
// rows with DeepSeek events gain an "ext" object and the report a top-level
// "ext". Rows are ranked by effective cost (session rows always, period rows
// with --top); with --top the upstream report is re-read without top so a
// DeepSeek row the catalog left unpriced can rank. Both reads must cover the
// same window. An ext failure never fails the command.
func forkMergeUsageReport(ctx context.Context, c *commandContext, upstream json.RawMessage, params url.Values, by string, top int) (json.RawMessage, error) {
	extRaw, ext, status, err := forkReadExt(ctx, c, params)
	if status != "ok" {
		return forkWithExtStatus(upstream, status, err)
	}
	if top > 0 && ext.Totals.PricedEvents > 0 {
		full := url.Values{}
		for k, v := range params {
			full[k] = v
		}
		full.Del("top")
		var res json.RawMessage
		if err := c.getJSON(ctx, apiPath("usage/report", full), &res); err != nil {
			return forkWithExtStatus(upstream, "error", fmt.Errorf("re-read the report for ranking: %w", err))
		}
		if err := checkReportAnswer(res, by, 0); err != nil {
			return forkWithExtStatus(upstream, "error", err)
		}
		upstream = res
	}
	// The two reads compute their windows from the clock; across a period
	// rollover they can differ. Read the extension once more, then give up.
	if !forkSameWindow(upstream, ext) {
		if extRaw, ext, status, err = forkReadExt(ctx, c, params); status != "ok" {
			return forkWithExtStatus(upstream, status, err)
		}
		if !forkSameWindow(upstream, ext) {
			return forkWithExtStatus(upstream, "error", errors.New("report and report-ext cover different windows (period rollover); run it again"))
		}
	}
	return forkMergeExt(upstream, extRaw, ext, by, top)
}

func forkReadExt(ctx context.Context, c *commandContext, params url.Values) (json.RawMessage, forkReportExt, string, error) {
	var ext forkReportExt
	raw, status, err := forkFetchReportExt(ctx, c, params)
	if status != "ok" {
		return nil, ext, status, err
	}
	if err := json.Unmarshal(raw, &ext); err != nil {
		return nil, ext, "error", fmt.Errorf("decode report-ext: %w", err)
	}
	return raw, ext, "ok", nil
}

// forkSameWindow reports whether the upstream report covers ext's window. A
// report without since/until (an old shape) is not compared.
func forkSameWindow(upstream json.RawMessage, ext forkReportExt) bool {
	var w struct {
		Since *time.Time `json:"since"`
		Until *time.Time `json:"until"`
	}
	if json.Unmarshal(upstream, &w) != nil || w.Since == nil || w.Until == nil {
		return true
	}
	return w.Since.Equal(ext.Since) && w.Until.Equal(ext.Until)
}

func forkWithExtStatus(upstream json.RawMessage, status string, err error) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if jsonErr := json.Unmarshal(upstream, &doc); jsonErr != nil {
		return nil, fmt.Errorf("decode usage report: %w", jsonErr)
	}
	ext := map[string]string{"status": status}
	if status == "error" && err != nil {
		ext["reason"] = err.Error()
	}
	b, _ := json.Marshal(ext)
	doc["ext"] = b
	return json.Marshal(doc)
}

// forkMergeExt attaches ext to the upstream rows (period mode) or session
// rows (session mode) and, with top, re-ranks and truncates them.
func forkMergeExt(upstream, extRaw json.RawMessage, ext forkReportExt, by string, top int) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(upstream, &doc); err != nil {
		return nil, fmt.Errorf("decode usage report: %w", err)
	}
	field, keyOf, exts := "rows", forkPeriodKey, forkPeriodExts(ext)
	if by == "session" {
		field, keyOf, exts = "sessionRows", forkSessionKey, forkSessionExts(ext)
	}
	var rows []map[string]json.RawMessage
	if raw, ok := doc[field]; ok {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("decode usage report %s: %w", field, err)
		}
	}
	rank := make([]int64, len(rows))
	for i, row := range rows {
		var cost int64
		_ = json.Unmarshal(row["costNanos"], &cost)
		rank[i] = cost
		if x, ok := exts[keyOf(row)]; ok {
			x.finish()
			if x.PricedEvents > 0 {
				effective := cost - x.ReplacedCatalogCostNanos + forkDeref(x.PricebookCostNanos)
				x.EffectiveCostNanos, rank[i] = &effective, effective
			}
			row["ext"] = mustJSON(x)
		}
	}
	var extDoc map[string]json.RawMessage
	if err := json.Unmarshal(extRaw, &extDoc); err != nil {
		return nil, fmt.Errorf("decode report-ext: %w", err)
	}
	extDoc["status"] = mustJSON("ok")
	if top > 0 || by == "session" {
		order := make([]int, len(rows))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return rank[order[a]] > rank[order[b]] })
		keep := len(order)
		if top > 0 {
			keep = min(top, keep)
			doc["top"] = mustJSON(top)
		}
		ranked := make([]map[string]json.RawMessage, 0, keep)
		for _, i := range order[:keep] {
			ranked = append(ranked, rows[i])
		}
		rows = ranked
		extDoc["rankedBy"] = mustJSON("ext.effectiveCostNanos for rows with priced DeepSeek events, costNanos otherwise")
	}
	if rows != nil {
		doc[field] = mustJSON(rows)
	}
	doc["ext"] = mustJSON(extDoc)
	return json.Marshal(doc)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func forkRowString(row map[string]json.RawMessage, name string) string {
	var s string
	_ = json.Unmarshal(row[name], &s)
	return s
}

func forkPeriodKey(row map[string]json.RawMessage) string {
	return forkRowKey(forkRowString(row, "period"), forkRowString(row, "harness"), forkRowString(row, "model"), forkRowString(row, "speed"))
}

func forkSessionKey(row map[string]json.RawMessage) string {
	return forkRowKey(forkRowString(row, "sessionId"), forkRowString(row, "harness"), forkRowString(row, "model"), forkRowString(row, "speed"))
}

func forkPeriodExts(ext forkReportExt) map[string]*forkRowExt {
	out := map[string]*forkRowExt{}
	for _, r := range ext.PricedRows {
		k := forkRowKey(r.Period, r.Harness, r.Model, r.Speed)
		x := out[k]
		if x == nil {
			x = &forkRowExt{WindowBasis: &forkWindowBasis{}}
			out[k] = x
		}
		x.note(r.BilledAs, r.Route, r.PriceVersion, r.BillingStatus, r.Events, r.BoundaryTokens, r.forkCosts)
		x.WindowBasis.ReplyTime += r.WindowBasis.ReplyTime
		x.WindowBasis.RequestStart += r.WindowBasis.RequestStart
		x.WindowBasis.ReplyTimeUnverified += r.WindowBasis.ReplyTimeUnverified
	}
	return out
}

func forkSessionExts(ext forkReportExt) map[string]*forkRowExt {
	out := map[string]*forkRowExt{}
	for _, r := range ext.SessionRows {
		k := forkRowKey(r.SessionID, r.Harness, r.Model, r.Speed)
		x := out[k]
		if x == nil {
			x = &forkRowExt{}
			out[k] = x
		}
		for status, n := range r.BillingStatuses {
			x.note(r.BilledAs, "", "", status, n, 0, forkCosts{})
		}
		x.BoundaryTokens += r.BoundaryTokens
		x.add(r.forkCosts)
	}
	return out
}

// forkMergeFleetUsageReport merges each host's report-ext into its report
// with the same per-host contract as a single-host run, every read going to
// that host. A host that cannot be reached again keeps its upstream answer
// with ext "error"; nothing here fails the run.
func forkMergeFleetUsageReport(ctx context.Context, c *commandContext, hosts []fleetUsageHost, params url.Values, by string, top int) error {
	health, contexts, fleetErr := c.fleetHosts(ctx)
	byHost := map[string]*commandContext{}
	for i, h := range health {
		byHost[h.Name+"\x00"+h.URL] = contexts[i]
	}
	var wg sync.WaitGroup
	for i := range hosts {
		if hosts[i].Status != remoteStatusOK {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			hc := byHost[hosts[i].Name+"\x00"+hosts[i].URL]
			var merged json.RawMessage
			var err error
			switch {
			case fleetErr != nil:
				merged, err = forkWithExtStatus(hosts[i].Data, "error", fleetErr)
			case hc == nil:
				merged, err = forkWithExtStatus(hosts[i].Data, "error", errors.New("host unreachable for the report-ext read"))
			default:
				merged, err = forkMergeUsageReport(ctx, hc, hosts[i].Data, params, by, top)
			}
			if err != nil {
				hosts[i].Status, hosts[i].Error, hosts[i].Data = remoteStatusError, err.Error(), nil
				return
			}
			hosts[i].Data = merged
		}()
	}
	wg.Wait()
	return nil
}

// forkHoursArgs are the `ao usage report --by hour` inputs.
type forkHoursArgs struct {
	days, offset int
	tz, project  string
	fleet, json  bool
}

// runForkUsageReportHours prints the DeepSeek hour-of-day histogram: 24 UTC
// buckets over the window, peak hours marked. Windows are UTC; --tz only adds
// a local-time column.
func runForkUsageReportHours(cmd *cobra.Command, c *commandContext, a forkHoursArgs) error {
	var display *time.Location
	if a.tz != "" {
		loc, err := time.LoadLocation(a.tz)
		if err != nil {
			return usageError{fmt.Errorf("unknown --tz %q", a.tz)}
		}
		display = loc
	}
	params := url.Values{}
	if a.days > 0 {
		params.Set("days", strconv.Itoa(a.days))
	}
	if a.offset > 0 {
		params.Set("offset", strconv.Itoa(a.offset))
	}
	if a.project != "" {
		params.Set("projectId", a.project)
	}
	path := apiPath(forkReportExtPath, params)
	out := cmd.OutOrStdout()
	if a.fleet {
		hosts, err := c.fleetGetJSON(cmd.Context(), path)
		if err != nil {
			return err
		}
		for i := range hosts {
			if hosts[i].Status != remoteStatusOK {
				continue
			}
			view, err := forkHoursView(hosts[i].Data, a.tz, display)
			if err != nil {
				hosts[i].Status, hosts[i].Error, hosts[i].Data = remoteStatusError, err.Error(), nil
				continue
			}
			hosts[i].Data = mustJSON(view)
		}
		if a.json {
			return writeJSON(out, map[string]any{"hosts": hosts})
		}
		for i, h := range hosts {
			if i > 0 {
				_, _ = fmt.Fprintln(out)
			}
			label := h.Name
			if !h.Local {
				label += " " + h.URL
			}
			_, _ = fmt.Fprintf(out, "== %s: %s ==\n", label, h.Status)
			if h.Status != remoteStatusOK {
				_, _ = fmt.Fprintf(out, "  %s\n", orDash(h.Error))
				continue
			}
			var view forkHoursReport
			if err := json.Unmarshal(h.Data, &view); err != nil {
				return err
			}
			if err := writeForkHours(out, view, display); err != nil {
				return err
			}
		}
		return nil
	}
	var raw json.RawMessage
	if err := c.getJSON(cmd.Context(), path, &raw); err != nil {
		var apiErr apiResponseError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return errors.New("this daemon has no DeepSeek report extension (--by hour); update it")
		}
		return err
	}
	view, err := forkHoursView(raw, a.tz, display)
	if err != nil {
		return err
	}
	if a.json {
		return writeJSON(out, view)
	}
	return writeForkHours(out, view, display)
}

// forkHoursHour is one bucket of `--by hour --json`: the report-ext hour plus
// the display-zone label when --tz is set.
type forkHoursHour struct {
	forkHour
	Display string `json:"display,omitempty"`
}

// forkHoursReport is the stable `ao usage report --by hour --json` shape.
type forkHoursReport struct {
	By              string           `json:"by"`
	Provider        string           `json:"provider"`
	Days            int              `json:"days"`
	Since           time.Time        `json:"since"`
	Until           time.Time        `json:"until"`
	WindowsTimeZone string           `json:"windowsTimeZone"`
	DisplayTimeZone string           `json:"displayTimeZone,omitempty"`
	DisplayOffsetAt *time.Time       `json:"displayOffsetAt,omitempty"`
	WindowRule      string           `json:"windowRule"`
	BoundaryRule    string           `json:"boundaryRule"`
	PeakRule        *forkPeakRule    `json:"peakRule"`
	PriceVersions   []string         `json:"priceVersions"`
	Hours           []forkHoursHour  `json:"hours"`
	Totals          forkReportTotals `json:"totals"`
	PriceBook       *forkBookRef     `json:"priceBook"`
	Warnings        []string         `json:"warnings"`
}

func forkHoursView(raw json.RawMessage, tz string, display *time.Location) (forkHoursReport, error) {
	var ext forkReportExt
	if err := json.Unmarshal(raw, &ext); err != nil {
		return forkHoursReport{}, fmt.Errorf("decode report-ext: %w", err)
	}
	if len(ext.Hours) != 24 {
		return forkHoursReport{}, fmt.Errorf("report-ext returned %d hour buckets, want 24", len(ext.Hours))
	}
	view := forkHoursReport{
		By: "hour", Provider: "deepseek", Days: ext.Days, Since: ext.Since, Until: ext.Until, WindowsTimeZone: "UTC",
		WindowRule: ext.WindowRule, BoundaryRule: ext.BoundaryRule, PeakRule: ext.PeakRule, PriceVersions: ext.PriceVersions,
		Totals: ext.Totals, PriceBook: ext.PriceBook, Warnings: ext.Warnings,
	}
	if view.PriceVersions == nil {
		view.PriceVersions = []string{}
	}
	if view.Warnings == nil {
		view.Warnings = []string{}
	}
	// The display offset is the zone's offset at the window's last instant;
	// a DST switch inside the window shifts earlier days by an hour.
	at := ext.Until.Add(-time.Second).UTC()
	if display != nil {
		view.DisplayTimeZone, view.DisplayOffsetAt = tz, &at
	}
	for _, h := range ext.Hours {
		hh := forkHoursHour{forkHour: h}
		if display != nil {
			hh.Display = time.Date(at.Year(), at.Month(), at.Day(), h.Hour, 0, 0, 0, time.UTC).In(display).Format("15:04")
		}
		view.Hours = append(view.Hours, hh)
	}
	return view, nil
}

func forkUSD(nanos int64) string { return fmt.Sprintf("$%.2f", float64(nanos)/1e9) }

func forkUSDPtr(p *int64) string {
	if p == nil {
		return "-"
	}
	return forkUSD(*p)
}

func forkCount(n int64) string {
	switch {
	case n >= 10_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return strconv.FormatInt(n, 10)
	}
}

const forkBarWidth = 20

func writeForkHours(out io.Writer, v forkHoursReport, display *time.Location) error {
	_, _ = fmt.Fprintf(out, "DeepSeek usage by hour of day (UTC), %s .. %s UTC (%d days)\n",
		v.Since.UTC().Format("2006-01-02 15:04"), v.Until.UTC().Format("2006-01-02 15:04"), v.Days)
	var maxCost, maxTokens int64
	for _, h := range v.Hours {
		maxCost = max(maxCost, forkDeref(h.PricebookCostNanos))
		maxTokens = max(maxTokens, h.total())
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	header := "HOUR UTC\t"
	if display != nil {
		header += strings.ToUpper(v.DisplayTimeZone) + "\t"
	}
	header += "PEAK\tEVENTS\tCACHE HIT\tCACHE MISS\tOUTPUT\tCOST\tPEAK COST\t"
	_, _ = fmt.Fprintln(tw, header)
	for _, h := range v.Hours {
		line := h.UTC + "\t"
		if display != nil {
			line += h.Display + "\t"
		}
		mark := ""
		if h.PeakHour {
			mark = "*"
		}
		bar := ""
		switch {
		case maxCost > 0:
			bar = strings.Repeat("#", int((forkDeref(h.PricebookCostNanos)*forkBarWidth+maxCost-1)/maxCost))
		case maxTokens > 0:
			bar = strings.Repeat("#", int((h.total()*forkBarWidth+maxTokens-1)/maxTokens))
		}
		cost, peakCost := "-", "-"
		if h.Events > 0 {
			cost = forkUSDPtr(h.PricebookCostNanos)
			if h.Peak.Events > 0 {
				peakCost = forkUSD(h.Peak.CostNanos)
			}
		}
		line += fmt.Sprintf("%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s", mark, h.Events, forkCount(h.CacheHit), forkCount(h.CacheMiss), forkCount(h.Output), cost, peakCost, bar)
		_, _ = fmt.Fprintln(tw, line)
	}
	t := v.Totals
	total := "TOTAL\t"
	if display != nil {
		total += "\t"
	}
	total += fmt.Sprintf("\t%d\t%s\t%s\t%s\t%s\t%s\t", t.Events, forkCount(t.CacheHit), forkCount(t.CacheMiss), forkCount(t.Output),
		forkUSDPtr(t.PricebookCostNanos), forkUSD(t.Peak.CostNanos))
	_, _ = fmt.Fprintln(tw, total)
	if err := tw.Flush(); err != nil {
		return err
	}
	if r := v.PeakRule; r != nil {
		ranges := make([]string, 0, len(r.UTCRanges))
		for _, rg := range r.UTCRanges {
			ranges = append(ranges, rg[0]+"-"+rg[1])
		}
		days := strings.Join(r.Weekdays, ",")
		if r.ExcludeHolidays == "cn" {
			days += " except Chinese public holidays"
		}
		_, _ = fmt.Fprintf(out, "* peak hour: %s UTC on %s; events on other days in these hours are off-peak.\n", strings.Join(ranges, ", "), days)
	}
	if display != nil && v.DisplayOffsetAt != nil {
		_, _ = fmt.Fprintf(out, "%s times use the offset on %s; windows are UTC.\n", v.DisplayTimeZone, v.DisplayOffsetAt.In(display).Format("2006-01-02 MST"))
	}
	_, _ = fmt.Fprintf(out, "cost: %s with peak/off-peak windows (peak %s over %d events, off-peak %s over %d); %s at peak list prices\n",
		forkUSDPtr(t.PricebookCostNanos), forkUSD(t.Peak.CostNanos), t.Peak.Events, forkUSD(t.OffPeak.CostNanos), t.OffPeak.Events, forkUSDPtr(t.PeakListCostNanos))
	if unpriced := t.Events - t.PricedEvents; unpriced > 0 {
		statuses := make([]string, 0, len(t.BillingStatuses))
		for _, s := range sortedKeysInt(t.BillingStatuses) {
			if s != "priced" {
				statuses = append(statuses, fmt.Sprintf("%s %d", s, t.BillingStatuses[s]))
			}
		}
		_, _ = fmt.Fprintf(out, "unpriced: %d events (%s), tokens shown, cost unknown\n", unpriced, strings.Join(statuses, ", "))
	}
	_, _ = fmt.Fprintf(out, "window: %s; %d events by request start, %d unverified (%s boundary tokens)\n",
		v.WindowRule, t.WindowBasis.RequestStart, t.WindowBasis.ReplyTimeUnverified, forkCount(t.BoundaryTokens))
	if b := v.PriceBook; b != nil {
		line := "prices: " + b.Version + " (" + b.Source
		if b.Label != "" {
			line += ", " + b.Label
		}
		if b.Stale {
			line += ", stale"
		}
		_, _ = fmt.Fprintln(out, line+")")
		for _, w := range b.Warnings {
			_, _ = fmt.Fprintln(out, "warning: "+w)
		}
	}
	for _, w := range v.Warnings {
		_, _ = fmt.Fprintln(out, "warning: "+w)
	}
	return nil
}

func forkDeref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func sortedKeysInt(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// forkUsageReportHelp extends the report command's long help.
const forkUsageReportHelp = "DeepSeek events are priced at read time by the fork price book (peak/off-peak windows, price versions,\n" +
	"legacy model aliases) and merged under \"ext\": a top-level \"ext\" object and an \"ext\" object on each row with\n" +
	"DeepSeek events. --top then ranks those rows by their price-book cost. A daemon without the extension gives\n" +
	"\"ext\": {\"status\": \"unsupported\"}. --by hour prints DeepSeek usage per UTC hour of day (peak hours marked;\n" +
	"--tz adds a local-time column only, windows stay UTC); add --json for JSON."
