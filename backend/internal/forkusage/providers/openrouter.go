package providers

import (
	"encoding/json"
	"strconv"
	"time"
)

// KeyInfo is the OpenRouter key's limit and usage as /api/v1/key reports it.
// Amounts are the provider's JSON numbers (credits = USD), passed through.
// Usage periods are UTC: the day, the week from Monday, the month.
type KeyInfo struct {
	LimitUSD        *json.Number `json:"limitUsd"`
	LimitReset      *string      `json:"limitReset"`
	RemainingUSD    *json.Number `json:"remainingUsd"`
	RemainingPct    *float64     `json:"remainingPct,omitempty"`
	UsageUSD        *json.Number `json:"usageUsd,omitempty"`
	UsageDailyUSD   *json.Number `json:"usageDailyUsd,omitempty"`
	UsageWeeklyUSD  *json.Number `json:"usageWeeklyUsd,omitempty"`
	UsageMonthlyUSD *json.Number `json:"usageMonthlyUsd,omitempty"`
	IsFreeTier      *bool        `json:"isFreeTier,omitempty"`
	Budget          *Budget      `json:"budget,omitempty"`
	// ResetsAt is when the limit resets: the key budget's resets_at, else
	// derived from limit_reset (daily, weekly from Monday, monthly; UTC).
	ResetsAt      *time.Time `json:"resetsAt,omitempty"`
	ResetInterval string     `json:"resetInterval,omitempty"`
	ResetBasis    string     `json:"resetBasis,omitempty"`
}

// Budget is the key's effective budget, when OpenRouter reports one.
type Budget struct {
	LimitUSD      *json.Number `json:"limitUsd,omitempty"`
	SpendUSD      *json.Number `json:"spendUsd,omitempty"`
	RemainingUSD  *json.Number `json:"remainingUsd,omitempty"`
	ResetInterval *string      `json:"resetInterval,omitempty"`
	ResetsAt      *time.Time   `json:"resetsAt,omitempty"`
}

type openrouterKeyResponse struct {
	Data *struct {
		Limit           *json.Number `json:"limit"`
		LimitReset      *string      `json:"limit_reset"`
		LimitRemaining  *json.Number `json:"limit_remaining"`
		Usage           *json.Number `json:"usage"`
		UsageDaily      *json.Number `json:"usage_daily"`
		UsageWeekly     *json.Number `json:"usage_weekly"`
		UsageMonthly    *json.Number `json:"usage_monthly"`
		IsFreeTier      *bool        `json:"is_free_tier"`
		EffectiveBudget *struct {
			LimitUSD      *json.Number `json:"limit_usd"`
			SpendUSD      *json.Number `json:"spend_usd"`
			RemainingUSD  *json.Number `json:"remaining_usd"`
			ResetInterval *string      `json:"reset_interval"`
			ResetsAt      *string      `json:"resets_at"`
		} `json:"effective_budget"`
	} `json:"data"`
}

// parseKey validates the /api/v1/key answer. The key's label (a masked
// key) and anything else not listed here are dropped.
func parseKey(resp openrouterKeyResponse, now time.Time) (KeyInfo, bool) {
	d := resp.Data
	if d == nil {
		return KeyInfo{}, false
	}
	for _, n := range []*json.Number{d.Limit, d.LimitRemaining, d.Usage, d.UsageDaily, d.UsageWeekly, d.UsageMonthly} {
		if !validNumber(n) {
			return KeyInfo{}, false
		}
	}
	k := KeyInfo{
		LimitUSD: d.Limit, LimitReset: shortString(d.LimitReset), RemainingUSD: d.LimitRemaining,
		UsageUSD: d.Usage, UsageDailyUSD: d.UsageDaily, UsageWeeklyUSD: d.UsageWeekly, UsageMonthlyUSD: d.UsageMonthly,
		IsFreeTier: d.IsFreeTier,
	}
	if b := d.EffectiveBudget; b != nil {
		for _, n := range []*json.Number{b.LimitUSD, b.SpendUSD, b.RemainingUSD} {
			if !validNumber(n) {
				return KeyInfo{}, false
			}
		}
		k.Budget = &Budget{LimitUSD: b.LimitUSD, SpendUSD: b.SpendUSD, RemainingUSD: b.RemainingUSD, ResetInterval: shortString(b.ResetInterval)}
		if b.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *b.ResetsAt); err == nil {
				t = t.UTC()
				k.Budget.ResetsAt = &t
			}
		}
	}
	if k.LimitUSD != nil && k.RemainingUSD != nil {
		limit, err1 := strconv.ParseFloat(string(*k.LimitUSD), 64)
		left, err2 := strconv.ParseFloat(string(*k.RemainingUSD), 64)
		if err1 == nil && err2 == nil && limit > 0 {
			pct := float64(int(left/limit*1000+0.5)) / 10
			k.RemainingPct = &pct
		}
	}
	switch {
	case k.Budget != nil && k.Budget.ResetsAt != nil:
		k.ResetsAt, k.ResetBasis = k.Budget.ResetsAt, "effective_budget.resets_at"
		if k.Budget.ResetInterval != nil {
			k.ResetInterval = *k.Budget.ResetInterval
		}
	case k.Budget != nil && k.Budget.ResetInterval != nil && nextReset(*k.Budget.ResetInterval, now) != nil:
		k.ResetInterval = *k.Budget.ResetInterval
		k.ResetsAt, k.ResetBasis = nextReset(k.ResetInterval, now), "effective_budget.reset_interval (derived, UTC)"
	case k.LimitReset != nil && nextReset(*k.LimitReset, now) != nil:
		k.ResetInterval = *k.LimitReset
		k.ResetsAt, k.ResetBasis = nextReset(k.ResetInterval, now), "limit_reset (derived, UTC)"
	}
	return k, true
}

func shortString(s *string) *string {
	if s == nil || len(*s) > 32 {
		return nil
	}
	return s
}

// nextReset is the next UTC reset instant for an OpenRouter reset interval:
// daily at midnight, weekly on Monday, monthly on the first.
func nextReset(interval string, now time.Time) *time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var t time.Time
	switch interval {
	case "daily", "day":
		t = day.AddDate(0, 0, 1)
	case "weekly", "week":
		offset := (8 - int(day.Weekday())) % 7
		if offset == 0 {
			offset = 7
		}
		t = day.AddDate(0, 0, offset)
	case "monthly", "month":
		t = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	default:
		return nil
	}
	return &t
}
