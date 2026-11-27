package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Fixed endpoints. The key is sent to these URLs only.
const (
	DeepSeekBalanceURL = "https://api.deepseek.com/user/balance"
	OpenRouterKeyURL   = "https://openrouter.ai/api/v1/key"
)

const (
	fetchTimeout = 10 * time.Second
	maxBodyBytes = 64 << 10
	userAgent    = "ao-provider-usage"
)

// Fixed failure reasons. Errors from the network stack, response bodies and
// headers are never surfaced: they can echo request data.
const (
	ReasonNoKey         = "no key"
	ReasonKeyFormat     = "key has an unusable format"
	ReasonInvalidKey    = "invalid key (401)"
	ReasonForbidden     = "key not permitted (403)"
	ReasonTimeout       = "timeout"
	ReasonNetwork       = "network error"
	ReasonUnexpected    = "unexpected response"
	ReasonRedirect      = "unexpected response (redirect refused)"
	ReasonTooLarge      = "unexpected response (body larger than 64 KB)"
	ReasonRateLimited   = "rate limited (429)"
	ReasonDisabled      = "polling disabled"
	reasonHTTPStatusFmt = "http status %d"
)

// newClient clones base and hardens it: no cookie jar, redirects refused,
// fixed timeout.
func newClient(base *http.Client, timeout time.Duration) *http.Client {
	c := &http.Client{}
	if base != nil {
		clone := *base
		c = &clone
	}
	c.Jar = nil
	c.Timeout = timeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// getJSON sends one authenticated GET and decodes a 200 answer into out. It
// returns a fixed reason on any failure.
func getJSON(ctx context.Context, client *http.Client, url, key string, out any) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil || req.URL.Scheme != "https" {
		return ReasonUnexpected
	}
	req.Header.Set("Authorization", "Bearer "+key)
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
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return ReasonRedirect
	case resp.StatusCode == http.StatusUnauthorized:
		return ReasonInvalidKey
	case resp.StatusCode == http.StatusForbidden:
		return ReasonForbidden
	case resp.StatusCode == http.StatusTooManyRequests:
		return ReasonRateLimited
	default:
		return fmt.Sprintf(reasonHTTPStatusFmt, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return ReasonTimeout
		}
		return ReasonUnexpected
	}
	if len(body) > maxBodyBytes {
		return ReasonTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return ReasonUnexpected
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ReasonUnexpected
	}
	return ""
}

func keyReason(err error) string {
	if errors.Is(err, errKeyInvalid) {
		return ReasonKeyFormat
	}
	return ReasonNoKey
}

// decimalPattern accepts plain decimals only: no exponent, no sign other
// than a leading minus, no empty string.
var decimalPattern = regexp.MustCompile(`^-?\d{1,15}(\.\d{1,12})?$`)

func parseDecimal(s string) (*big.Rat, bool) {
	if !decimalPattern.MatchString(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func scale(s string) int {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

// formatDecimal renders r with the given number of decimals (at least 2).
func formatDecimal(r *big.Rat, decimals int) string {
	if decimals < 2 {
		decimals = 2
	}
	return r.FloatString(decimals)
}

// numberPattern is a JSON number as OpenRouter sends it.
var numberPattern = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][-+]?\d+)?$`)

func validNumber(n *json.Number) bool {
	return n == nil || numberPattern.MatchString(string(*n))
}
