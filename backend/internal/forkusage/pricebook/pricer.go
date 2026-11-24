package pricebook

import (
	"errors"
	"math/big"
	"sort"
	"strings"
	"time"
)

// Billing statuses of a priced event.
const (
	StatusPriced          = "priced"
	StatusNoPriceEvidence = "no-price-evidence"
	StatusUnknownModel    = "unknown-model"
	StatusUnknownRoute    = "unknown-route"
)

// Usage is one reply's tokens and the instant that decides its window.
type Usage struct {
	Route string
	Model string
	// At is the window instant: the reply time, or the request start for a
	// request that spans a window boundary (see WindowRule).
	At              time.Time
	CacheHitTokens  int64
	CacheMissTokens int64
	OutputTokens    int64
}

// Priced is the read-time price of one Usage. Costs are nano-USD, each
// component rounded half-up; TotalNanos is the sum of the rounded components,
// so totals add up across rows.
type Priced struct {
	Status         string `json:"billingStatus"`
	Model          string `json:"model"`
	BilledAs       string `json:"billedAs,omitempty"`
	Version        string `json:"priceVersion,omitempty"`
	Window         string `json:"window,omitempty"`
	CacheHitNanos  int64  `json:"cacheHitNanos"`
	CacheMissNanos int64  `json:"cacheMissNanos"`
	OutputNanos    int64  `json:"outputNanos"`
	TotalNanos     int64  `json:"costNanos"`
	// HolidayYearMissing marks a peak price applied on a weekday whose
	// holiday calendar is not transcribed (an upper bound).
	HolidayYearMissing bool `json:"holidayYearMissing,omitempty"`
}

var (
	errNegativeTokens = errors.New("negative token count")
	errOverflow       = errors.New("cost overflows int64 nano-USD")
	thousand          = big.NewRat(1000, 1) // USD per 1M tokens -> nano-USD per token
)

// Price prices one event at read time. Unknown routes and models, and events
// before the first evidenced version, are reported with their status and no
// cost; they are never guessed.
func (b *Book) Price(u Usage) (Priced, error) {
	p := Priced{Model: u.Model}
	if u.CacheHitTokens < 0 || u.CacheMissTokens < 0 || u.OutputTokens < 0 {
		return p, errNegativeTokens
	}
	if b.RouteBilling(u.Route) != "api" {
		p.Status = StatusUnknownRoute
		return p, nil
	}
	s := b.stateAt(u.At)
	if s.noEvidence {
		p.Status = StatusNoPriceEvidence
		return p, nil
	}
	v := s.version
	billed := u.Model
	if target, ok := v.Aliases[billed]; ok {
		billed = target
	}
	m, ok := v.models[billed]
	if !ok {
		p.Status = StatusUnknownModel
		return p, nil
	}
	rates := m.offPeak
	p.Window = WindowOffPeak
	if s.peak {
		rates, p.Window = m.peak, WindowPeak
		p.HolidayYearMissing = s.yearMissing
	}
	p.Status, p.BilledAs, p.Version = StatusPriced, billed, v.Version.Version
	var err error
	if p.CacheHitNanos, err = componentNanos(u.CacheHitTokens, rates[0]); err != nil {
		return p, err
	}
	if p.CacheMissNanos, err = componentNanos(u.CacheMissTokens, rates[1]); err != nil {
		return p, err
	}
	if p.OutputNanos, err = componentNanos(u.OutputTokens, rates[2]); err != nil {
		return p, err
	}
	total := new(big.Int).SetInt64(p.CacheHitNanos)
	total.Add(total, big.NewInt(p.CacheMissNanos))
	total.Add(total, big.NewInt(p.OutputNanos))
	if !total.IsInt64() {
		return p, errOverflow
	}
	p.TotalNanos = total.Int64()
	return p, nil
}

// componentNanos is tokens × USD-per-1M, in nano-USD, rounded half-up
// (matching upstream pricing.roundNanoUSD).
func componentNanos(tokens int64, perM *big.Rat) (int64, error) {
	nanos := new(big.Rat).Mul(new(big.Rat).SetInt64(tokens), perM)
	nanos.Mul(nanos, thousand)
	q, r := new(big.Int).QuoRem(nanos.Num(), nanos.Denom(), new(big.Int))
	if new(big.Int).Lsh(r, 1).Cmp(nanos.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, errOverflow
	}
	return q.Int64(), nil
}

// ModelRates are one model's rates in a version as decimal strings.
type ModelRates struct {
	Model         string `json:"model"`
	PeakPerM      Rates  `json:"peakPerM"`
	OffPeakPerM   Rates  `json:"offPeakPerM"`
	RetiredOnPage bool   `json:"retiredOnPage,omitempty"`
}

// Rates lists the rates of the version in effect at t (false before the
// first evidenced version), sorted by model.
func (b *Book) Rates(t time.Time) ([]ModelRates, bool) {
	v, ok := b.versionAt(t)
	if !ok {
		return nil, false
	}
	return v.rates(), true
}

func (c *compiled) rates() []ModelRates {
	out := make([]ModelRates, 0, len(c.models))
	for name, m := range c.models {
		out = append(out, ModelRates{Model: name, PeakPerM: ratesOf(m.peak), OffPeakPerM: ratesOf(m.offPeak), RetiredOnPage: m.retired})
	}
	sortRates(out)
	return out
}

func ratesOf(r [3]*big.Rat) Rates {
	return Rates{CacheHit: decimalString(r[0]), CacheMiss: decimalString(r[1]), Output: decimalString(r[2])}
}

// decimalString renders r as a plain decimal without trailing zeros.
func decimalString(r *big.Rat) string {
	s := r.FloatString(12)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func sortRates(rs []ModelRates) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].Model < rs[j].Model })
}
