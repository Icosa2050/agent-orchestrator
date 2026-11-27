package providers

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Europe/Berlin in packaged builds.

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
)

// SteeringSchema versions the `steering` block.
const SteeringSchema = 1

// Advice codes (design §9.2). Precedence: stop > low-runway > window/limit
// codes. `unknown` never means "go": treat it as peak / tight budget.
const (
	AdviceStop           = "stop"
	AdviceLowRunway      = "low-runway"
	AdviceDeferNonurgent = "defer-nonurgent"
	AdviceOKOffPeak      = "ok-offpeak"
	AdvicePreferCheaper  = "prefer-cheaper"
	AdviceOK             = "ok"
	AdviceUnknown        = "unknown"
)

// AdviceCodes lists every code, for docs checks.
var AdviceCodes = []string{AdviceStop, AdviceLowRunway, AdviceDeferNonurgent, AdviceOKOffPeak, AdvicePreferCheaper, AdviceOK, AdviceUnknown}

// Runway and ranking parameters.
const (
	runwayWindow     = 7 * 24 * time.Hour
	runwayMinHistory = 2 * 24 * time.Hour
	// blendInput:blendOutput is the fallback token mix for blendedPerM until
	// the PAL ledger supplies an observed one.
	blendInput  = 4
	blendOutput = 1
	// BlendBasis states the mix behind blendedPerM.
	BlendBasis = "fallback 4:1 input:output (no PAL ledger yet)"
	// CrossRouteNote qualifies alsoVia: information, never advice.
	CrossRouteNote = "information only: OpenRouter serves these from other hosts with other data policies; moving DeepSeek work there is an owner decision"
)

var berlinLoc = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return time.UTC
	}
	return loc
}()

func berlinString(t time.Time) string { return t.In(berlinLoc).Format(time.RFC3339) }

// Steering is the machine-readable steering block of `ao usage limits
// --json`: one entry per provider. Signals and advice only; nothing is
// blocked.
type Steering struct {
	Schema     int                 `json:"schema"`
	DeepSeek   *DeepSeekSteering   `json:"deepseek,omitempty"`
	OpenRouter *OpenRouterSteering `json:"openrouter,omitempty"`
	Thresholds SteeringThresholds  `json:"thresholds"`
}

// SteeringThresholds are the thresholds the advice used.
type SteeringThresholds struct {
	Thresholds
	PreferCheaperPct float64 `json:"preferCheaperPct"`
}

// Advice is a deterministic code with a human reason.
type Advice struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// SteerSpan is a UTC interval with its Europe/Berlin rendering.
type SteerSpan struct {
	From        time.Time `json:"from"`
	Until       time.Time `json:"until"`
	FromBerlin  string    `json:"fromBerlin"`
	UntilBerlin string    `json:"untilBerlin"`
}

func steerSpan(s *pricebook.SpanPair) *SteerSpan {
	if s == nil {
		return nil
	}
	return &SteerSpan{From: s.From.UTC, Until: s.Until.UTC, FromBerlin: s.From.Berlin, UntilBerlin: s.Until.Berlin}
}

// Runway is balance (or remaining limit) over recent average daily spend.
// Days is null with a reason when it cannot be derived.
type Runway struct {
	Days          *float64 `json:"days"`
	AvgDailySpend string   `json:"avgDailySpend,omitempty"`
	Currency      string   `json:"currency,omitempty"`
	Basis         string   `json:"basis,omitempty"`
	CoverageDays  *float64 `json:"coverageDays,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	// raw is the unrounded Days the thresholds compare against.
	raw *float64
}

// SteerBalance is the DeepSeek wallet the advice used.
type SteerBalance struct {
	Amount      string     `json:"amount"`
	Currency    string     `json:"currency"`
	IsAvailable *bool      `json:"isAvailable,omitempty"`
	Source      string     `json:"source"`
	FetchedAt   *time.Time `json:"fetchedAt,omitempty"`
	Stale       bool       `json:"stale,omitempty"`
}

// SteerBook names the price book behind the window.
type SteerBook struct {
	Version     string     `json:"version"`
	Source      string     `json:"source"`
	LastCheckAt *time.Time `json:"lastCheckAt,omitempty"`
	Stale       bool       `json:"stale"`
}

// AlsoVia lists the same vendor's models on another route (cross-route
// observation, owner decision 14: information only).
type AlsoVia struct {
	Route    string        `json:"route"`
	Currency string        `json:"currency"`
	Note     string        `json:"note"`
	ListedAt *time.Time    `json:"listedAt,omitempty"`
	Models   []ListedModel `json:"models"`
}

// DeepSeekSteering is the DeepSeek "price now" signal.
type DeepSeekSteering struct {
	KeyID string `json:"keyId,omitempty"`
	// Status and StatusReason are the balance row's (ok, disabled,
	// unavailable): an unavailable row's reading is the last good one.
	Status             string                  `json:"status"`
	StatusReason       string                  `json:"statusReason,omitempty"`
	Hosts              []string                `json:"hosts,omitempty"`
	FetchedAt          *time.Time              `json:"fetchedAt,omitempty"`
	Window             string                  `json:"window"`
	PriceFactor        *float64                `json:"priceFactor"`
	PeakVsOffPeak      *float64                `json:"peakVsOffPeak"`
	WindowEndsAt       *time.Time              `json:"windowEndsAt,omitempty"`
	WindowEndsAtBerlin string                  `json:"windowEndsAtBerlin,omitempty"`
	NextOffPeak        *SteerSpan              `json:"nextOffPeak,omitempty"`
	NextPeak           *SteerSpan              `json:"nextPeak,omitempty"`
	Holiday            string                  `json:"holiday,omitempty"`
	WindowRule         string                  `json:"windowRule,omitempty"`
	HolidayCalendar    string                  `json:"holidayCalendar,omitempty"`
	Models             []pricebook.WindowModel `json:"models"`
	AlsoVia            *AlsoVia                `json:"alsoVia,omitempty"`
	Balance            *SteerBalance           `json:"balance"`
	Runway             Runway                  `json:"runway"`
	Advice             Advice                  `json:"advice"`
	PriceBook          *SteerBook              `json:"priceBook,omitempty"`
}

// SteerKey is the binding OpenRouter limit: the effective budget or the key
// limit, whichever has the smaller remaining share.
type SteerKey struct {
	KeyID          string     `json:"keyId,omitempty"`
	RemainingUSD   float64    `json:"remainingUsd"`
	LimitUSD       float64    `json:"limitUsd"`
	RemainingPct   float64    `json:"remainingPct"`
	LimitBasis     string     `json:"limitBasis"`
	ResetInterval  string     `json:"resetInterval,omitempty"`
	ResetsAt       *time.Time `json:"resetsAt,omitempty"`
	ResetsAtBerlin string     `json:"resetsAtBerlin,omitempty"`
	// share is the unrounded remaining fraction (0-1) the thresholds use.
	share float64
}

// SteerModel is one PAL-allowed model's listed price and rank (1 =
// cheapest by blendedPerM). Status "not listed" models have no price and
// no rank.
type SteerModel struct {
	Model          string `json:"model"`
	Status         string `json:"status"`
	PromptPerM     string `json:"promptPerM,omitempty"`
	CompletionPerM string `json:"completionPerM,omitempty"`
	CacheReadPerM  string `json:"cacheReadPerM,omitempty"`
	BlendedPerM    string `json:"blendedPerM,omitempty"`
	Rank           int    `json:"rank,omitempty"`
}

// SteerPreset is an allowed OpenRouter preset with its chain as PAL's model
// registry describes it. Informational: OpenRouter resolves presets
// server-side and they are not priced themselves.
type SteerPreset struct {
	Model  string       `json:"model"`
	Name   string       `json:"name"`
	Chain  []SteerModel `json:"chain"`
	Source string       `json:"source"`
}

// Model statuses in the steering list.
const (
	ModelListed    = "listed"
	ModelNotListed = "not listed"
	ModelUnpriced  = "no listing"
)

// OpenRouterSteering is the PAL/OpenRouter signal.
type OpenRouterSteering struct {
	KeyID string `json:"keyId,omitempty"`
	// Status and StatusReason are the key row's; Stale marks a key reading
	// older than a failed refresh.
	Status              string        `json:"status"`
	StatusReason        string        `json:"statusReason,omitempty"`
	Stale               bool          `json:"stale,omitempty"`
	Hosts               []string      `json:"hosts,omitempty"`
	FetchedAt           *time.Time    `json:"fetchedAt,omitempty"`
	TimePricing         string        `json:"timePricing"`
	Key                 *SteerKey     `json:"key"`
	Runway              Runway        `json:"runway"`
	Models              []SteerModel  `json:"models"`
	Presets             []SteerPreset `json:"presets"`
	BlendBasis          string        `json:"blendBasis"`
	AllowedModelsSource string        `json:"allowedModelsSource"`
	AllowedModelsNote   string        `json:"allowedModelsNote,omitempty"`
	ListingStatus       string        `json:"listingStatus"`
	ListingReason       string        `json:"listingReason,omitempty"`
	ListedAt            *time.Time    `json:"listedAt,omitempty"`
	ListingSource       string        `json:"listingSource"`
	Advice              Advice        `json:"advice"`
}

// steerInputs are everything the advice is computed from.
type steerInputs struct {
	now        time.Time
	th         Thresholds
	window     *pricebook.WindowReport
	deepseek   Row
	openrouter Row
	samples    []Sample
	samplesErr bool
	listing    listingFile
	listingOK  bool
	listingOn  bool
	pal        PALConfig
}

func buildSteering(in steerInputs) Steering {
	return Steering{
		Schema:     SteeringSchema,
		DeepSeek:   deepseekSteering(in),
		OpenRouter: openrouterSteering(in),
		Thresholds: SteeringThresholds{Thresholds: in.th, PreferCheaperPct: PreferCheaperPct},
	}
}

func parseFactor(s string) *float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || s == "" {
		return nil
	}
	return &f
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func deepseekSteering(in steerInputs) *DeepSeekSteering {
	d := &DeepSeekSteering{Models: []pricebook.WindowModel{}, Runway: Runway{}}
	row := in.deepseek
	if w := in.window; w != nil {
		d.Window, d.Holiday, d.WindowRule, d.HolidayCalendar = w.Window, w.Holiday, w.WindowRule, w.HolidayCalendar
		d.PriceFactor, d.PeakVsOffPeak = parseFactor(w.PriceFactor), parseFactor(w.PeakVsOffPeak)
		if w.WindowEndsAt != nil {
			t := w.WindowEndsAt.UTC
			d.WindowEndsAt, d.WindowEndsAtBerlin = &t, w.WindowEndsAt.Berlin
		}
		d.NextOffPeak, d.NextPeak = steerSpan(w.NextOffPeak), steerSpan(w.NextPeak)
		if w.Models != nil {
			d.Models = w.Models
		}
		d.PriceBook = &SteerBook{Version: w.PriceBook.Version, Source: w.PriceBook.Source, LastCheckAt: w.PriceBook.LastCheckAt, Stale: w.PriceBook.Stale}
	}
	if in.listingOK {
		var ds []ListedModel
		for _, m := range in.listing.Models {
			if strings.HasPrefix(m.ID, "deepseek/") || strings.HasPrefix(m.ID, "~deepseek/") {
				ds = append(ds, m)
			}
		}
		if len(ds) > 0 {
			at := in.listing.FetchedAt
			d.AlsoVia = &AlsoVia{Route: "openrouter", Currency: "USD", Note: CrossRouteNote, ListedAt: &at, Models: ds}
		}
	}
	d.Status, d.StatusReason = row.Status, row.Reason
	if d.Status == "" {
		d.Status = StatusDisabled
	}
	var balance *big.Rat
	if row.Status != StatusDisabled && row.Balance != nil {
		d.KeyID, d.FetchedAt = row.KeyID, row.FetchedAt
		d.Balance = &SteerBalance{Amount: row.Balance.Total, Currency: row.Balance.Currency, IsAvailable: row.IsAvailable,
			Source: "user/balance", FetchedAt: row.FetchedAt, Stale: row.Stale}
		balance, _ = parseDecimal(row.Balance.Total)
	}
	switch {
	case row.Status == StatusDisabled:
		d.Runway.Reason = "balance polling disabled"
	case balance == nil && row.Reason != "":
		d.Runway.Reason = "no balance reading: " + row.Reason
	case balance == nil:
		d.Runway.Reason = "no balance reading"
	case in.samplesErr:
		d.Runway.Reason = "balance history unreadable"
	default:
		d.Runway = deepseekRunway(in.samples, row.KeyID, row.Balance.Currency, balance, in.now)
	}
	d.Advice = deepseekAdvice(d, balance, in.th, row)
	return d
}

// deepseekRunway derives the runway from the balance history of one key and
// currency over the last 7 days. Spend is the total_balance decrease per
// sample interval; intervals with a top-up or grant are excluded (a deposit
// would hide spend). AO-session spend alone is never used: both DeepSeek
// routes and tools outside AO share the wallet.
func deepseekRunway(samples []Sample, keyID, currency string, balance *big.Rat, now time.Time) Runway {
	r := Runway{Currency: currency, Basis: "balance change, last 7 d, adjustment intervals excluded"}
	start := now.Add(-runwayWindow)
	var in []Sample
	for _, s := range samples {
		if s.KeyID == keyID && s.Currency == currency && !s.TS.Before(start) && !s.TS.After(now) {
			in = append(in, s)
		}
	}
	spend := new(big.Rat)
	var covered time.Duration
	for i := 1; i < len(in); i++ {
		a, b := in[i-1], in[i]
		if increased(a.ToppedUp, b.ToppedUp) || increased(a.Granted, b.Granted) {
			continue
		}
		covered += b.TS.Sub(a.TS)
		x, _ := parseDecimal(a.Total)
		y, _ := parseDecimal(b.Total)
		if d := new(big.Rat).Sub(x, y); d.Sign() > 0 {
			spend.Add(spend, d)
		}
	}
	coverage := round1(covered.Hours() / 24)
	r.CoverageDays = &coverage
	if covered < runwayMinHistory {
		r.Reason = "insufficient history"
		return r
	}
	avg := new(big.Rat).Quo(spend, new(big.Rat).SetFloat64(covered.Hours()/24))
	r.AvgDailySpend = avg.FloatString(2)
	if spend.Sign() == 0 {
		r.Reason = "no spend observed"
		return r
	}
	raw, _ := new(big.Rat).Quo(balance, avg).Float64()
	raw = math.Max(raw, 0)
	days := round1(raw)
	r.Days, r.raw = &days, &raw
	return r
}

func deepseekAdvice(d *DeepSeekSteering, balance *big.Rat, th Thresholds, row Row) Advice {
	cur := ""
	if balance != nil {
		cur = row.Balance.Currency + " " + row.Balance.Total
	}
	switch {
	case row.IsAvailable != nil && !*row.IsAvailable:
		return Advice{AdviceStop, "DeepSeek reports the account unavailable (is_available=false)"}
	case balance != nil && balance.Sign() <= 0:
		return Advice{AdviceStop, "balance is " + cur}
	}
	low, _ := new(big.Rat).SetString(th.BalanceLow)
	switch {
	case d.Runway.raw != nil && *d.Runway.raw < th.RunwayLowDays:
		return Advice{AdviceLowRunway, fmt.Sprintf("runway %s d (below %s d); balance %s", fmtDays(*d.Runway.Days), fmtDays(th.RunwayLowDays), cur)}
	case balance != nil && low != nil && balance.Cmp(low) < 0:
		return Advice{AdviceLowRunway, fmt.Sprintf("balance %s is below %s", cur, th.BalanceLow)}
	}
	note := ""
	switch {
	case balance == nil:
		note = "; balance unknown (" + d.Runway.Reason + ")"
	case row.Status != StatusOK && row.Reason != "":
		note = "; last balance refresh failed (" + row.Reason + "), balance is the last good reading"
	}
	switch {
	case d.Window == "":
		return Advice{AdviceUnknown, "no price window (price book unavailable); treat as peak" + note}
	case d.PriceBook != nil && d.PriceBook.Stale:
		return Advice{AdviceUnknown, "price book is stale; " + d.Window + " shown, treat as peak" + note}
	case strings.Contains(d.HolidayCalendar, "missing"):
		return Advice{AdviceUnknown, "holiday calendar " + d.HolidayCalendar + "; treat as peak" + note}
	case d.Window == pricebook.WindowPeak:
		reason := "peak"
		if d.WindowEndsAt != nil {
			reason += " until " + clockPair(*d.WindowEndsAt)
		}
		if d.NextOffPeak != nil && d.WindowEndsAt != nil && d.NextOffPeak.From.Equal(*d.WindowEndsAt) {
			reason += "; off-peak is " + cheaper(d.PeakVsOffPeak)
		}
		return Advice{AdviceDeferNonurgent, reason + note}
	default:
		reason := "off-peak (" + cheaper(d.PeakVsOffPeak) + ")"
		if d.WindowEndsAt != nil {
			reason += " until " + clockPair(*d.WindowEndsAt)
		}
		return Advice{AdviceOKOffPeak, reason + note}
	}
}

func cheaper(peakVsOffPeak *float64) string {
	if peakVsOffPeak == nil {
		return "cheaper; see the per-model rates"
	}
	if *peakVsOffPeak == 2 {
		return "half price"
	}
	return "1/" + strconv.FormatFloat(*peakVsOffPeak, 'f', -1, 64) + " of the peak price"
}

func fmtDays(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func clockPair(t time.Time) string {
	return t.UTC().Format("Mon 15:04") + " UTC (" + t.In(berlinLoc).Format("15:04") + " Berlin)"
}

func numberValue(n *json.Number) (float64, bool) {
	if n == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(*n), 64)
	return f, err == nil
}

// bindingLimit picks the limit with the smaller remaining share.
func bindingLimit(k *KeyInfo) *SteerKey {
	var best *SteerKey
	consider := func(limit, remaining *json.Number, basis, interval string, resets *time.Time) {
		l, ok1 := numberValue(limit)
		r, ok2 := numberValue(remaining)
		if !ok1 || !ok2 || l < 0 {
			return
		}
		// A zero limit is an exhausted budget, not "no limit".
		share := 0.0
		if l > 0 && r > 0 {
			share = r / l
		}
		c := &SteerKey{RemainingUSD: r, LimitUSD: l, RemainingPct: round1(share * 100), LimitBasis: basis, ResetInterval: interval, ResetsAt: resets, share: share}
		if c.ResetsAt != nil {
			c.ResetsAtBerlin = berlinString(*c.ResetsAt)
		}
		if best == nil || c.share < best.share || (c.share == best.share && c.RemainingUSD < best.RemainingUSD) {
			best = c
		}
	}
	if b := k.Budget; b != nil {
		interval := ""
		if b.ResetInterval != nil {
			interval = *b.ResetInterval
		}
		consider(b.LimitUSD, b.RemainingUSD, "effective_budget", interval, b.ResetsAt)
	}
	interval := ""
	if k.LimitReset != nil {
		interval = *k.LimitReset
	}
	consider(k.LimitUSD, k.RemainingUSD, "key limit", interval, k.LimitResetsAt)
	return best
}

func openrouterSteering(in steerInputs) *OpenRouterSteering {
	o := &OpenRouterSteering{
		TimePricing: "none known", Models: []SteerModel{}, Presets: []SteerPreset{}, BlendBasis: BlendBasis,
		AllowedModelsSource: in.pal.Source, AllowedModelsNote: in.pal.Reason, ListingSource: ListingSource,
	}
	switch {
	case in.listingOK && len(in.listing.Models) > 0:
		o.ListingStatus = ListingOK
		at := in.listing.FetchedAt
		o.ListedAt = &at
		if !in.listingOn {
			o.ListingReason = "listing refresh is off; cached listing shown"
		} else if in.listing.LastReason != "" {
			o.ListingReason = "last refresh failed: " + in.listing.LastReason
		}
	case !in.listingOn:
		o.ListingStatus, o.ListingReason = ListingDisabled, "enable with `ao usage providers set modelsListing=on`"
	case in.listingOK && in.listing.LastReason != "":
		o.ListingStatus, o.ListingReason = ListingUnavailable, in.listing.LastReason
	default:
		o.ListingStatus = ListingNotFetched
	}
	byID := map[string]ListedModel{}
	if o.ListingStatus == ListingOK {
		for _, m := range in.listing.Models {
			byID[m.ID] = m
		}
	}
	for _, name := range in.pal.Allowed {
		if presetPattern.MatchString(name) {
			p := SteerPreset{Model: name, Name: strings.TrimPrefix(name, "@preset/"), Chain: []SteerModel{}, Source: "PAL model registry description (OPENROUTER_MODELS_CONFIG_PATH)"}
			for _, link := range in.pal.Presets[name] {
				p.Chain = append(p.Chain, steerModel(link, byID, o.ListingStatus))
			}
			o.Presets = append(o.Presets, p)
			continue
		}
		o.Models = append(o.Models, steerModel(name, byID, o.ListingStatus))
	}
	rankModels(o.Models)
	row := in.openrouter
	o.Status, o.StatusReason, o.Stale = row.Status, row.Reason, row.Stale
	if o.Status == "" {
		o.Status = StatusDisabled
	}
	if row.Status != StatusDisabled && row.Key != nil {
		o.KeyID, o.FetchedAt = row.KeyID, row.FetchedAt
		o.Key = bindingLimit(row.Key)
		if o.Key != nil {
			o.Key.KeyID = row.KeyID
		}
	}
	o.Runway = openrouterRunway(row, o.Key, in.now)
	o.Advice = openrouterAdvice(o, row, in.th)
	return o
}

func steerModel(name string, byID map[string]ListedModel, listing string) SteerModel {
	m, ok := byID[name]
	switch {
	case ok:
		return SteerModel{Model: name, Status: ModelListed, PromptPerM: m.PromptPerM, CompletionPerM: m.CompletionPerM, CacheReadPerM: m.CacheReadPerM, BlendedPerM: blended(m)}
	case listing == ListingOK:
		return SteerModel{Model: name, Status: ModelNotListed}
	default:
		return SteerModel{Model: name, Status: ModelUnpriced}
	}
}

// blended is (4 x prompt + 1 x completion) / 5 per million tokens.
func blended(m ListedModel) string {
	p, ok1 := new(big.Rat).SetString(m.PromptPerM)
	c, ok2 := new(big.Rat).SetString(m.CompletionPerM)
	if !ok1 || !ok2 {
		return ""
	}
	sum := new(big.Rat).Add(new(big.Rat).Mul(p, big.NewRat(blendInput, 1)), new(big.Rat).Mul(c, big.NewRat(blendOutput, 1)))
	return trimDecimal(sum.Quo(sum, big.NewRat(blendInput+blendOutput, 1)), 6)
}

// rankModels ranks listed models by blendedPerM (ties by name); the list
// keeps the allowed-list order.
func rankModels(ms []SteerModel) {
	idx := []int{}
	for i, m := range ms {
		if m.BlendedPerM != "" {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, _ := new(big.Rat).SetString(ms[idx[a]].BlendedPerM)
		y, _ := new(big.Rat).SetString(ms[idx[b]].BlendedPerM)
		if c := x.Cmp(y); c != 0 {
			return c < 0
		}
		return ms[idx[a]].Model < ms[idx[b]].Model
	})
	for r, i := range idx {
		ms[i].Rank = r + 1
	}
}

// Cheapest is the rank-1 model, if any.
func (o *OpenRouterSteering) Cheapest() *SteerModel {
	for i := range o.Models {
		if o.Models[i].Rank == 1 {
			return &o.Models[i]
		}
	}
	return nil
}

// openrouterRunway is the remaining limit over the week's average daily
// spend (usage_weekly / elapsed UTC week days, at least one day).
func openrouterRunway(row Row, key *SteerKey, now time.Time) Runway {
	r := Runway{Currency: "USD", Basis: "usage_weekly / elapsed UTC week days (at least 1)"}
	switch {
	case row.Status == StatusDisabled:
		r.Reason = "key polling disabled"
		return r
	case row.Key == nil:
		r.Reason = "no key reading"
		return r
	case key == nil:
		r.Reason = "key has no limit"
		return r
	}
	weekly, ok := numberValue(row.Key.UsageWeeklyUSD)
	if !ok {
		r.Reason = "no weekly usage reported"
		return r
	}
	// usage_weekly is as of the reading, not now (a stale reading would
	// otherwise spread one day's spend over more days).
	if row.FetchedAt != nil {
		now = *row.FetchedAt
	}
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	elapsed := math.Max(now.Sub(monday).Hours()/24, 1)
	avg := weekly / elapsed
	r.AvgDailySpend = strconv.FormatFloat(avg, 'f', 2, 64)
	if avg <= 0 {
		r.Reason = "no spend this week"
		return r
	}
	raw := math.Max(key.RemainingUSD/avg, 0)
	days := round1(raw)
	r.Days, r.raw = &days, &raw
	return r
}

func openrouterAdvice(o *OpenRouterSteering, row Row, th Thresholds) Advice {
	if row.Status == StatusDisabled || row.Key == nil {
		why := "key not read"
		if row.Status == StatusDisabled {
			why = "key polling disabled; enable with `ao usage providers enable openrouter`"
		} else if row.Reason != "" {
			why = "key not read: " + row.Reason
		}
		return Advice{AdviceUnknown, why + "; treat the budget as tight"}
	}
	failed := ""
	if row.Status != StatusOK && row.Reason != "" {
		failed = "last refresh failed (" + row.Reason + "); "
	}
	k := o.Key
	if k == nil {
		if rem, ok := numberValue(row.Key.RemainingUSD); ok && rem <= 0 {
			return Advice{AdviceStop, failed + fmt.Sprintf("$%.2f left", rem)}
		}
		if failed != "" {
			return Advice{AdviceUnknown, failed + "key has no limit; treat the budget as tight"}
		}
		return Advice{AdviceOK, "key has no limit"}
	}
	left := fmt.Sprintf("$%.2f of $%.2f left", k.RemainingUSD, k.LimitUSD)
	if k.ResetInterval != "" {
		left += " (" + k.ResetInterval + ")"
	}
	if k.ResetsAt != nil {
		left += "; resets " + clockPair(*k.ResetsAt)
	}
	cheapest := ""
	if c := o.Cheapest(); c != nil {
		cheapest = "; use " + c.Model
	}
	// A failed refresh keeps the cautionary codes of the last reading but
	// never says "ok" on it.
	left = failed + left
	switch {
	case k.RemainingUSD <= 0:
		return Advice{AdviceStop, left}
	case o.Runway.raw != nil && *o.Runway.raw < th.RunwayLowDays:
		return Advice{AdviceLowRunway, fmt.Sprintf("runway %s d (below %s d); %s%s", fmtDays(*o.Runway.Days), fmtDays(th.RunwayLowDays), left, cheapest)}
	case k.share*100 < th.KeyRemainingLowPct:
		return Advice{AdviceLowRunway, fmt.Sprintf("%s%% left (below %s%%); %s%s", fmtDays(k.RemainingPct), fmtDays(th.KeyRemainingLowPct), left, cheapest)}
	case k.share*100 < PreferCheaperPct:
		return Advice{AdvicePreferCheaper, fmt.Sprintf("%s%% left (below %d%%); %s", fmtDays(k.RemainingPct), PreferCheaperPct, left)}
	case failed != "":
		return Advice{AdviceUnknown, left + "; treat the budget as tight"}
	}
	return Advice{AdviceOK, left}
}

// BorrowRunway fills d's missing runway from another host's reading of the
// same wallet (fleet view: d's host has too little balance history). d stays
// authoritative: its balance, availability, status and advice are kept, the
// runway is other's average daily spend against d's balance, and the
// borrowed runway can only tighten the advice (window codes to low-runway),
// never loosen stop or low-runway.
func (d *DeepSeekSteering) BorrowRunway(other *DeepSeekSteering, host string, th Thresholds) {
	if d == nil || other == nil || d.Runway.Days != nil || other.Runway.Days == nil || d.Balance == nil ||
		other.Runway.Currency != d.Balance.Currency || other.Runway.AvgDailySpend == "" {
		return
	}
	balance, ok := parseDecimal(d.Balance.Amount)
	avg, ok2 := new(big.Rat).SetString(other.Runway.AvgDailySpend)
	if !ok || !ok2 || avg.Sign() <= 0 {
		return
	}
	raw, _ := new(big.Rat).Quo(balance, avg).Float64()
	raw = math.Max(raw, 0)
	days := round1(raw)
	d.Runway = Runway{Days: &days, AvgDailySpend: other.Runway.AvgDailySpend, Currency: other.Runway.Currency,
		Basis: other.Runway.Basis + "; spend history from " + host, CoverageDays: other.Runway.CoverageDays, raw: &raw}
	if d.Advice.Code == AdviceStop || d.Advice.Code == AdviceLowRunway || raw >= th.RunwayLowDays {
		return
	}
	d.Advice = Advice{AdviceLowRunway, fmt.Sprintf("runway %s d (below %s d; spend history from %s); balance %s %s",
		fmtDays(days), fmtDays(th.RunwayLowDays), host, d.Balance.Currency, d.Balance.Amount)}
}
