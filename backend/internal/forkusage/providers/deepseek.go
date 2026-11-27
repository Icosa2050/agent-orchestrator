package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/forkusage/pricebook"
)

// Balance history limits.
const (
	historyRetention = 90 * 24 * time.Hour
	changeWindow     = 24 * time.Hour
	maxSampleLine    = 4 << 10
	maxHistoryBytes  = 32 << 20
)

// BalanceHistoryPath is the DeepSeek balance sample log.
func BalanceHistoryPath(dataDir string) string {
	return filepath.Join(filepath.Dir(pricebook.ConfigPath(dataDir)), "deepseek-balance.jsonl")
}

// Balance is one currency's DeepSeek balance, amounts as decimal strings in
// that currency. CNY and USD are never mixed or converted.
type Balance struct {
	Currency string `json:"currency"`
	Total    string `json:"total"`
	Granted  string `json:"granted"`
	ToppedUp string `json:"toppedUp"`
}

// Sample is one stored balance reading.
type Sample struct {
	TS       time.Time `json:"ts"`
	KeyID    string    `json:"keyId"`
	Currency string    `json:"currency"`
	Total    string    `json:"total"`
	Granted  string    `json:"granted"`
	ToppedUp string    `json:"toppedUp"`
}

// Coverage describes the samples behind a change.
type Coverage struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Samples int       `json:"samples"`
	// Gaps counts sample intervals longer than twice the poll interval.
	Gaps int `json:"gaps"`
	// Partial is true when the first sample is more than two poll intervals
	// after the start of the window.
	Partial bool `json:"partial"`
}

// Adjustment is a balance increase that is not spend: a top-up or a grant.
type Adjustment struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
}

// Change is the observed change of total_balance over the window.
type Change struct {
	Window      string       `json:"window"`
	Currency    string       `json:"currency"`
	Amount      *string      `json:"amount"`
	Basis       string       `json:"basis"`
	Reason      string       `json:"reason,omitempty"`
	Coverage    *Coverage    `json:"coverage,omitempty"`
	Adjustments []Adjustment `json:"adjustments"`
}

// OutsideAO is the spend outside AO sessions. It stays null until it is
// resolvable (no adjustments, every AO DeepSeek event priced, every host
// sharing the key reporting); the AO-session join arrives with the report
// extension, so PR C always reports the reason.
type OutsideAO struct {
	Window   string  `json:"window"`
	Amount   *string `json:"amount"`
	Currency string  `json:"currency"`
	Reason   string  `json:"reason"`
}

type deepseekBalanceResponse struct {
	IsAvailable  *bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string `json:"currency"`
		TotalBalance    string `json:"total_balance"`
		GrantedBalance  string `json:"granted_balance"`
		ToppedUpBalance string `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// parseBalance validates the /user/balance answer: amounts must be plain
// decimal strings ("42.10"; "" and "1e3" are rejected).
func parseBalance(resp deepseekBalanceResponse) (bool, []Balance, bool) {
	if resp.IsAvailable == nil || len(resp.BalanceInfos) == 0 || len(resp.BalanceInfos) > 8 {
		return false, nil, false
	}
	seen := map[string]bool{}
	out := make([]Balance, 0, len(resp.BalanceInfos))
	for _, b := range resp.BalanceInfos {
		if !currencyPattern.MatchString(b.Currency) || seen[b.Currency] {
			return false, nil, false
		}
		seen[b.Currency] = true
		for _, v := range []string{b.TotalBalance, b.GrantedBalance, b.ToppedUpBalance} {
			if _, ok := parseDecimal(v); !ok {
				return false, nil, false
			}
		}
		out = append(out, Balance{Currency: b.Currency, Total: b.TotalBalance, Granted: b.GrantedBalance, ToppedUp: b.ToppedUpBalance})
	}
	return *resp.IsAvailable, out, true
}

// primaryBalance is the currency the account actually holds money in: the
// first listed with a non-zero total, else the first listed.
func primaryBalance(bs []Balance) Balance {
	for _, b := range bs {
		if r, ok := parseDecimal(b.Total); ok && r.Sign() != 0 {
			return b
		}
	}
	return bs[0]
}

// readSamples reads the history, skipping malformed and oversized lines
// (a partial trailing line from a crash included).
func readSamples(dataDir string) ([]Sample, error) {
	path := BalanceHistoryPath(dataDir)
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.Size() > maxHistoryBytes {
		return nil, errors.New("balance history too large")
	}
	data, err := os.ReadFile(path) //nolint:gosec // fork-owned file under the data dir
	if err != nil {
		return nil, err
	}
	var out []Sample
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 || len(line) > maxSampleLine {
			continue
		}
		var s Sample
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if dec.Decode(&s) != nil || s.TS.IsZero() || s.KeyID == "" || !currencyPattern.MatchString(s.Currency) {
			continue
		}
		if _, ok := parseDecimal(s.Total); !ok {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// appendSamples adds samples and drops readings older than the retention.
// The file is rewritten atomically when something expires, else appended.
func appendSamples(dataDir string, now time.Time, add []Sample) error {
	path := BalanceHistoryPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	existing, err := readSamples(dataDir)
	if err != nil {
		existing = nil // An unreadable history restarts; it holds no secrets.
	}
	cutoff := now.Add(-historyRetention)
	kept := existing[:0]
	for _, s := range existing {
		if !s.TS.Before(cutoff) {
			kept = append(kept, s)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err != nil || len(kept) != len(existing) || !endsWithNewline(path) {
		for _, s := range append(kept, add...) {
			_ = enc.Encode(s)
		}
		return writeFileAtomic(path, buf.Bytes())
	}
	for _, s := range add {
		_ = enc.Encode(s)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // fork-owned file under the data dir
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// endsWithNewline is false for a file whose last line is partial (a crash
// mid-write); appending to it would corrupt the new line too.
func endsWithNewline(path string) bool {
	f, err := os.Open(path) //nolint:gosec // fork-owned file under the data dir
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	if st.Size() == 0 {
		return true
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, st.Size()-1); err != nil {
		return false
	}
	return b[0] == '\n'
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// computeChange derives the observed change of one key's balance in one
// currency over the 24 h before now. Samples of another key or currency are
// ignored, so a key rotation starts a new baseline.
func computeChange(samples []Sample, keyID, currency string, now time.Time) Change {
	ch := Change{Window: "24h", Currency: currency, Basis: "observed total_balance change between samples", Adjustments: []Adjustment{}}
	start := now.Add(-changeWindow)
	tolerance := 2 * PollInterval
	var in []Sample
	var before *Sample
	for i := range samples {
		s := samples[i]
		if s.KeyID != keyID || s.Currency != currency || s.TS.After(now) {
			continue
		}
		if s.TS.Before(start) {
			before = &samples[i]
			continue
		}
		in = append(in, s)
	}
	// A reading just before the window is a better baseline than none.
	if before != nil && start.Sub(before.TS) <= tolerance {
		in = append([]Sample{*before}, in...)
	}
	if len(in) < 2 {
		ch.Reason = "insufficient history"
		return ch
	}
	cov := &Coverage{From: in[0].TS, To: in[len(in)-1].TS, Samples: len(in), Partial: in[0].TS.Sub(start) > tolerance}
	for i := 1; i < len(in); i++ {
		a, b := in[i-1], in[i]
		if b.TS.Sub(a.TS) > tolerance {
			cov.Gaps++
		}
		if increased(a.ToppedUp, b.ToppedUp) {
			ch.Adjustments = append(ch.Adjustments, Adjustment{At: b.TS, Kind: "top-up"})
		}
		if increased(a.Granted, b.Granted) {
			ch.Adjustments = append(ch.Adjustments, Adjustment{At: b.TS, Kind: "grant"})
		}
	}
	ch.Coverage = cov
	first, _ := parseDecimal(in[0].Total)
	last, _ := parseDecimal(in[len(in)-1].Total)
	amount := formatDecimal(new(big.Rat).Sub(last, first), max(scale(in[0].Total), scale(in[len(in)-1].Total)))
	ch.Amount = &amount
	if len(ch.Adjustments) > 0 {
		ch.Reason = "includes a deposit or grant; not spend alone"
	}
	return ch
}

func increased(from, to string) bool {
	a, okA := parseDecimal(from)
	b, okB := parseDecimal(to)
	return okA && okB && b.Cmp(a) > 0
}

func outsideAO(ch Change) OutsideAO {
	o := OutsideAO{Window: ch.Window, Currency: ch.Currency}
	switch {
	case ch.Amount == nil:
		o.Reason = ch.Reason
	case len(ch.Adjustments) > 0:
		o.Reason = "unresolved: " + ch.Adjustments[0].Kind + " detected " + ch.Adjustments[0].At.UTC().Format("Mon 15:04") + " UTC"
	default:
		o.Reason = "unresolved: AO session spend is not joined yet"
	}
	return o
}
