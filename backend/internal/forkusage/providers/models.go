package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// OpenRouterModelsURL is OpenRouter's public model listing. It is keyless:
// the request carries no Authorization header and no cookies.
const OpenRouterModelsURL = "https://openrouter.ai/api/v1/models"

// Listing cadence and limits.
const (
	// ListingEvery is the refresh age of the cached listing (owner decision
	// 10: at most daily).
	ListingEvery = 24 * time.Hour
	// listingRetry spaces attempts after a failed fetch.
	listingRetry     = time.Hour
	listingTimeout   = 20 * time.Second
	maxListingBytes  = 8 << 20
	maxListingModels = 4096
	maxPALConfig     = 16 << 20
)

// Listing statuses.
const (
	ListingOK          = "ok"
	ListingDisabled    = "disabled"
	ListingNotFetched  = "not fetched yet"
	ListingUnavailable = "unavailable"
)

// ListingSource names the listing in the steering block.
const ListingSource = "openrouter.ai/api/v1/models (public, keyless)"

// ListedModel is one model's listed price, in USD per million tokens as
// decimal strings. CacheReadPerM is empty when the listing has none.
type ListedModel struct {
	ID             string `json:"id"`
	PromptPerM     string `json:"promptPerM"`
	CompletionPerM string `json:"completionPerM"`
	CacheReadPerM  string `json:"cacheReadPerM,omitempty"`
}

// listingFile is the cached listing, reduced to the price fields.
type listingFile struct {
	FetchedAt   time.Time     `json:"fetchedAt"`
	LastAttempt time.Time     `json:"lastAttempt"`
	LastReason  string        `json:"lastReason,omitempty"`
	Models      []ListedModel `json:"models"`
}

// ListingPath is the cached OpenRouter model listing.
func ListingPath(dataDir string) string {
	return filepath.Join(filepath.Dir(BalanceHistoryPath(dataDir)), "openrouter-models.json")
}

func readListing(dataDir string) (listingFile, bool) {
	var f listingFile
	data, err := os.ReadFile(ListingPath(dataDir)) //nolint:gosec // fork-owned file under the data dir
	if err != nil || len(data) > maxListingBytes {
		return f, false
	}
	if json.Unmarshal(data, &f) != nil {
		return listingFile{}, false
	}
	return f, true
}

func writeListing(dataDir string, f listingFile) error {
	if err := os.MkdirAll(filepath.Dir(ListingPath(dataDir)), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return writeFileAtomic(ListingPath(dataDir), data)
}

type openrouterModelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing *struct {
			Prompt         *string `json:"prompt"`
			Completion     *string `json:"completion"`
			InputCacheRead *string `json:"input_cache_read"`
		} `json:"pricing"`
	} `json:"data"`
}

var modelIDPattern = regexp.MustCompile(`^~?[a-z0-9][a-z0-9._-]{0,63}/[A-Za-z0-9._:-]{1,96}$`)

// perTokenPattern is a listed per-token price: a plain non-negative decimal.
// OpenRouter lists "-1" for routers with variable prices; those are skipped.
var perTokenPattern = regexp.MustCompile(`^\d{1,6}(\.\d{1,18})?$`)

var million = big.NewRat(1_000_000, 1)

// perM converts a per-token price string to USD per million tokens.
func perM(s string) (string, bool) {
	if !perTokenPattern.MatchString(s) {
		return "", false
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", false
	}
	return trimDecimal(new(big.Rat).Mul(r, million), 9), true
}

// trimDecimal renders r with at most decimals places and no trailing zeros.
func trimDecimal(r *big.Rat, decimals int) string {
	s := r.FloatString(decimals)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// parseListing validates the listing and keeps the priced models.
func parseListing(resp openrouterModelsResponse) ([]ListedModel, bool) {
	if len(resp.Data) == 0 {
		return nil, false
	}
	out := make([]ListedModel, 0, len(resp.Data))
	seen := map[string]bool{}
	for _, m := range resp.Data {
		if len(out) >= maxListingModels {
			break
		}
		if !modelIDPattern.MatchString(m.ID) || seen[m.ID] || m.Pricing == nil || m.Pricing.Prompt == nil || m.Pricing.Completion == nil {
			continue
		}
		prompt, ok1 := perM(*m.Pricing.Prompt)
		completion, ok2 := perM(*m.Pricing.Completion)
		if !ok1 || !ok2 {
			continue
		}
		lm := ListedModel{ID: m.ID, PromptPerM: prompt, CompletionPerM: completion}
		if m.Pricing.InputCacheRead != nil {
			if c, ok := perM(*m.Pricing.InputCacheRead); ok {
				lm.CacheReadPerM = c
			}
		}
		seen[m.ID] = true
		out = append(out, lm)
	}
	if len(out) == 0 {
		return nil, false
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, true
}

// getPublicJSON sends one keyless GET (no Authorization, no cookies,
// redirects refused) and decodes a 200 answer. It returns a fixed reason on
// any failure.
func getPublicJSON(ctx context.Context, client *http.Client, url string, limit int64, out any) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil || req.URL.Scheme != "https" {
		return ReasonUnexpected
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return ReasonTimeout
		}
		return ReasonNetwork
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return ReasonRedirect
	case resp.StatusCode == http.StatusTooManyRequests:
		return ReasonRateLimited
	default:
		return fmt.Sprintf(reasonHTTPStatusFmt, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ReasonTimeout
		}
		return ReasonUnexpected
	}
	if int64(len(body)) > limit {
		return "unexpected response (body too large)"
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(out); err != nil {
		return ReasonUnexpected
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ReasonUnexpected
	}
	return ""
}

// PALConfig is what AO reads from the PAL MCP server's configuration: the
// allowed model names and the preset chains PAL's own model registry
// describes. Names only; PAL's environment holds no key in these entries.
type PALConfig struct {
	// Source names where the allowed list came from.
	Source  string
	Allowed []string
	// Presets maps "@preset/<name>" to its model chain, in order.
	Presets map[string][]string
	Reason  string
}

// PALConfigPaths locates the PAL MCP config (~/.claude.json, key
// mcpServers.pal.env.OPENROUTER_ALLOWED_MODELS).
type PALConfigPaths struct {
	ClaudeJSON string // default ~/.claude.json
}

const palAllowedSource = "~/.claude.json mcpServers.pal.env.OPENROUTER_ALLOWED_MODELS"

// readPALConfig reads the allowed list and the preset chains. Missing files
// leave the list empty with a reason; it never fails the caller.
func readPALConfig(paths PALConfigPaths) PALConfig {
	cfg := PALConfig{Source: palAllowedSource, Presets: map[string][]string{}}
	path := paths.ClaudeJSON
	home, _ := os.UserHomeDir()
	if path == "" {
		if home == "" {
			cfg.Reason = "no home directory"
			return cfg
		}
		path = filepath.Join(home, ".claude.json")
	}
	data, err := readCapped(path, maxPALConfig)
	if err != nil {
		cfg.Reason = "PAL MCP config not readable"
		if errors.Is(err, fs.ErrNotExist) {
			cfg.Reason = "no PAL MCP config (~/.claude.json)"
		}
		return cfg
	}
	// Decode only the pal server's two names: other servers and other env
	// values may have any shape and are never kept.
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &doc) != nil {
		cfg.Reason = "PAL MCP config not readable"
		return cfg
	}
	raw, ok := doc.MCPServers["pal"]
	if !ok {
		cfg.Reason = "no pal server in the MCP config"
		return cfg
	}
	var server struct {
		Env map[string]json.RawMessage `json:"env"`
	}
	_ = json.Unmarshal(raw, &server)
	env := func(name string) string {
		var v string
		if r, ok := server.Env[name]; ok && json.Unmarshal(r, &v) == nil {
			return v
		}
		return ""
	}
	seen := map[string]bool{}
	for _, name := range strings.Split(env("OPENROUTER_ALLOWED_MODELS"), ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] || len(cfg.Allowed) >= 256 || (!modelIDPattern.MatchString(name) && !presetPattern.MatchString(name)) {
			continue
		}
		seen[name] = true
		cfg.Allowed = append(cfg.Allowed, name)
	}
	if len(cfg.Allowed) == 0 {
		cfg.Reason = "OPENROUTER_ALLOWED_MODELS is not set (PAL may call any model)"
	}
	if modelsPath := env("OPENROUTER_MODELS_CONFIG_PATH"); modelsPath != "" {
		if strings.HasPrefix(modelsPath, "~/") && home != "" {
			modelsPath = filepath.Join(home, modelsPath[2:])
		}
		if filepath.IsAbs(modelsPath) {
			cfg.Presets = readPresetChains(modelsPath)
		}
	}
	return cfg
}

var presetPattern = regexp.MustCompile(`^@preset/[a-z0-9][a-z0-9_-]{0,63}$`)

// presetDescription is how PAL's model registry describes a preset:
// "OpenRouter preset pal-cheap: a -> b -> c (…)".
var presetDescription = regexp.MustCompile(`^OpenRouter preset ([a-z0-9][a-z0-9_-]{0,63}): ([^()]+)`)

// readPresetChains reads the preset chains from PAL's custom model registry
// (OPENROUTER_MODELS_CONFIG_PATH). They are informational: OpenRouter
// resolves the preset server-side.
func readPresetChains(path string) map[string][]string {
	out := map[string][]string{}
	data, err := readCapped(path, 4<<20)
	if err != nil {
		return out
	}
	var reg struct {
		Models []struct {
			ModelName   string `json:"model_name"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if json.Unmarshal(data, &reg) != nil {
		return out
	}
	for _, m := range reg.Models {
		if !presetPattern.MatchString(m.ModelName) {
			continue
		}
		match := presetDescription.FindStringSubmatch(m.Description)
		if len(match) != 3 || "@preset/"+match[1] != m.ModelName {
			continue
		}
		var chain []string
		for _, part := range strings.Split(match[2], "->") {
			part = strings.TrimSpace(part)
			if !modelIDPattern.MatchString(part) {
				chain = nil
				break
			}
			chain = append(chain, part)
		}
		if len(chain) > 0 && len(chain) <= 16 {
			out[m.ModelName] = chain
		}
	}
	return out
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the owner's own config file, read only
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file too large")
	}
	return data, nil
}
