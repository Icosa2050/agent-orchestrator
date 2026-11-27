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
	"strings"

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

// Thresholds are the steering thresholds (owner decision 11) behind the
// advice codes (steering.go).
type Thresholds struct {
	RunwayLowDays      float64 `json:"runwayLowDays"`
	BalanceLow         string  `json:"balanceLow"`
	KeyRemainingLowPct float64 `json:"keyRemainingLowPct"`
}

// DefaultThresholds are the owner-approved defaults.
var DefaultThresholds = Thresholds{RunwayLowDays: 3, BalanceLow: "5", KeyRemainingLowPct: 25}

// PreferCheaperPct is the OpenRouter remaining share below which the advice
// is prefer-cheaper (owner decision 11, fixed).
const PreferCheaperPct = 50

type providerSetting struct {
	Enabled bool `json:"enabled"`
}

// fileConfig is this package's view of provider-usage/config.json. Other
// sections (the price book's) are preserved by pricebook.UpdateConfig.
type fileConfig struct {
	Providers  map[string]providerSetting `json:"providers"`
	Thresholds *json.RawMessage           `json:"thresholds"`
	Models     *struct {
		AutoFetch *bool `json:"autoFetch"`
	} `json:"openrouterModels"`
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
	enabled, th, _ := readSettings(dataDir)
	return enabled, th
}

// readSettings also returns the OpenRouter models-listing switch (default
// on, owner decision 10). An unreadable file keeps the listing on: it is
// keyless, like the price-page check.
func readSettings(dataDir string) (map[string]bool, Thresholds, bool) {
	enabled := map[string]bool{}
	th := DefaultThresholds
	listing := true
	data, err := os.ReadFile(pricebook.ConfigPath(dataDir))
	if err != nil {
		return enabled, th, listing
	}
	var cfg fileConfig
	if json.Unmarshal(data, &cfg) != nil {
		return enabled, th, listing
	}
	if cfg.Models != nil && cfg.Models.AutoFetch != nil {
		listing = *cfg.Models.AutoFetch
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
	return enabled, th, listing
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

// Settings is a partial update of the steering settings; nil fields stay.
type Settings struct {
	RunwayLowDays      *float64 `json:"runwayLowDays,omitempty"`
	BalanceLow         *string  `json:"balanceLow,omitempty"`
	KeyRemainingLowPct *float64 `json:"keyRemainingLowPct,omitempty"`
	ModelsListing      *bool    `json:"modelsListing,omitempty"`
}

var errBadSetting = errors.New("invalid setting")

// validate rejects values readSettings would ignore.
func (s Settings) validate() error {
	switch {
	case s.RunwayLowDays == nil && s.BalanceLow == nil && s.KeyRemainingLowPct == nil && s.ModelsListing == nil:
		return fmt.Errorf("%w: nothing to set", errBadSetting)
	case s.RunwayLowDays != nil && (*s.RunwayLowDays < 0 || *s.RunwayLowDays > 365):
		return fmt.Errorf("%w: runwayLowDays must be 0-365", errBadSetting)
	case s.BalanceLow != nil && (!decimalPattern.MatchString(*s.BalanceLow) || strings.HasPrefix(*s.BalanceLow, "-")):
		return fmt.Errorf("%w: balanceLow must be a plain non-negative decimal such as 5 or 2.50", errBadSetting)
	case s.KeyRemainingLowPct != nil && (*s.KeyRemainingLowPct < 0 || *s.KeyRemainingLowPct > 100):
		return fmt.Errorf("%w: keyRemainingLowPct must be 0-100", errBadSetting)
	}
	return nil
}

// writeSettings applies s, keeping every other setting.
func writeSettings(dataDir string, s Settings) error {
	if err := s.validate(); err != nil {
		return err
	}
	return pricebook.UpdateConfig(dataDir, func(cfg map[string]json.RawMessage) error {
		edit := func(key string, set func(map[string]json.RawMessage)) error {
			section := map[string]json.RawMessage{}
			if raw, ok := cfg[key]; ok {
				if err := json.Unmarshal(raw, &section); err != nil {
					return fmt.Errorf("provider-usage config %s section: %w", key, err)
				}
				if section == nil {
					section = map[string]json.RawMessage{}
				}
			}
			set(section)
			cfg[key], _ = json.Marshal(section)
			return nil
		}
		if s.RunwayLowDays != nil || s.BalanceLow != nil || s.KeyRemainingLowPct != nil {
			if err := edit("thresholds", func(m map[string]json.RawMessage) {
				if s.RunwayLowDays != nil {
					m["runwayLowDays"], _ = json.Marshal(*s.RunwayLowDays)
				}
				if s.BalanceLow != nil {
					m["balanceLow"], _ = json.Marshal(*s.BalanceLow)
				}
				if s.KeyRemainingLowPct != nil {
					m["keyRemainingLowPct"], _ = json.Marshal(*s.KeyRemainingLowPct)
				}
			}); err != nil {
				return err
			}
		}
		if s.ModelsListing != nil {
			return edit("openrouterModels", func(m map[string]json.RawMessage) {
				m["autoFetch"], _ = json.Marshal(*s.ModelsListing)
			})
		}
		return nil
	})
}
