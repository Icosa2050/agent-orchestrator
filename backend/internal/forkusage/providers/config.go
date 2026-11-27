// Package providers is the fork-only (Icosa2050, see FORK.md "Usage seams")
// provider polling for API-billed usage: the DeepSeek account balance from
// the official GET /user/balance and the OpenRouter key's limit and usage from
// GET /api/v1/key. Both are opt-in per provider and off by default. Keys are
// read per request from the owner's own credential stores, sent only to that
// provider's own host, and never retained, stored or logged.
package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
)

// Provider names.
const (
	DeepSeek   = "deepseek"
	OpenRouter = "openrouter"
)

// Known lists the pollable providers in display order.
var Known = []string{DeepSeek, OpenRouter}

// Row statuses.
const (
	StatusOK          = "ok"
	StatusDisabled    = "disabled"
	StatusUnavailable = "unavailable"
	StatusUnsupported = "unsupported"
)

// Thresholds are the steering thresholds (owner decision 11). PR C only
// reports them; the advice codes that use them arrive with the steering
// layer.
type Thresholds struct {
	RunwayLowDays      float64 `json:"runwayLowDays"`
	BalanceLow         string  `json:"balanceLow"`
	KeyRemainingLowPct float64 `json:"keyRemainingLowPct"`
}

// DefaultThresholds are the owner-approved defaults.
var DefaultThresholds = Thresholds{RunwayLowDays: 3, BalanceLow: "5", KeyRemainingLowPct: 25}

type providerSetting struct {
	Enabled bool `json:"enabled"`
}

// fileConfig is this package's view of provider-usage/config.json. Other
// sections (the price book's) are preserved by pricebook.UpdateConfig.
type fileConfig struct {
	Providers  map[string]providerSetting `json:"providers"`
	Thresholds *json.RawMessage           `json:"thresholds"`
}

// knownProvider reports whether name can be polled.
func knownProvider(name string) bool {
	for _, p := range Known {
		if p == name {
			return true
		}
	}
	return false
}

// readConfig returns the enabled switches and thresholds. A missing or
// unreadable file means every provider is off (the safe default).
func readConfig(dataDir string) (map[string]bool, Thresholds) {
	enabled := map[string]bool{}
	th := DefaultThresholds
	data, err := os.ReadFile(pricebook.ConfigPath(dataDir))
	if err != nil {
		return enabled, th
	}
	var cfg fileConfig
	if json.Unmarshal(data, &cfg) != nil {
		return enabled, th
	}
	for name, s := range cfg.Providers {
		if knownProvider(name) {
			enabled[name] = s.Enabled
		}
	}
	if cfg.Thresholds != nil {
		var over struct {
			RunwayLowDays      *float64 `json:"runwayLowDays"`
			BalanceLow         *string  `json:"balanceLow"`
			KeyRemainingLowPct *float64 `json:"keyRemainingLowPct"`
		}
		if json.Unmarshal(*cfg.Thresholds, &over) == nil {
			if over.RunwayLowDays != nil && *over.RunwayLowDays >= 0 {
				th.RunwayLowDays = *over.RunwayLowDays
			}
			if over.BalanceLow != nil && decimalPattern.MatchString(*over.BalanceLow) {
				th.BalanceLow = *over.BalanceLow
			}
			if over.KeyRemainingLowPct != nil && *over.KeyRemainingLowPct >= 0 && *over.KeyRemainingLowPct <= 100 {
				th.KeyRemainingLowPct = *over.KeyRemainingLowPct
			}
		}
	}
	return enabled, th
}

var errUnknownProvider = errors.New("unknown provider")

// writeEnabled switches one provider, keeping every other setting.
func writeEnabled(dataDir, provider string, on bool) error {
	if !knownProvider(provider) {
		return errUnknownProvider
	}
	return pricebook.UpdateConfig(dataDir, func(cfg map[string]json.RawMessage) error {
		section := map[string]json.RawMessage{}
		if raw, ok := cfg["providers"]; ok {
			if err := json.Unmarshal(raw, &section); err != nil {
				return fmt.Errorf("provider-usage config providers section: %w", err)
			}
			if section == nil { // "providers": null
				section = map[string]json.RawMessage{}
			}
		}
		entry := map[string]json.RawMessage{}
		if raw, ok := section[provider]; ok {
			if err := json.Unmarshal(raw, &entry); err != nil {
				return fmt.Errorf("provider-usage config %s section: %w", provider, err)
			}
			if entry == nil {
				entry = map[string]json.RawMessage{}
			}
		}
		entry["enabled"], _ = json.Marshal(on)
		section[provider], _ = json.Marshal(entry)
		cfg["providers"], _ = json.Marshal(section)
		return nil
	})
}
