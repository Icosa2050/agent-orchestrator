package acp

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func claudeLimitMeta(kind string, utilization float64, resetsAt time.Time) map[string]any {
	return map[string]any{"_claude/rateLimit": map[string]any{
		"rateLimitType": kind, "utilization": utilization, "resetsAt": float64(resetsAt.Unix()),
	}}
}

func TestClaudeRateLimitTypesFillTheirOwnWindow(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	weekly := now.Add(72 * time.Hour)
	session := now.Add(3 * time.Hour)
	c := &conversation{}

	// A weekly reading is not the session window, and its type is not a plan.
	limits := c.claudeRateLimits(claudeLimitMeta("seven_day", 0.84, weekly), now)
	if limits == nil || limits.PrimaryUsedPercent != -1 || limits.SecondaryUsedPercent != 84 || !limits.SecondaryResetsAt.Equal(weekly) || limits.PlanLabel != "" {
		t.Fatalf("seven_day = %#v", limits)
	}
	// The five-hour reading fills the session slot and keeps the weekly one.
	limits = c.claudeRateLimits(claudeLimitMeta("five_hour", 0.25, session), now)
	if limits.PrimaryUsedPercent != 25 || !limits.PrimaryResetsAt.Equal(session) || limits.SecondaryUsedPercent != 84 {
		t.Fatalf("five_hour = %#v", limits)
	}
	// A tighter per-model weekly window wins the weekly slot.
	modelReset := now.Add(24 * time.Hour)
	limits = c.claudeRateLimits(claudeLimitMeta("seven_day_opus", 0.9, modelReset), now)
	if limits.SecondaryUsedPercent != 90 || !limits.SecondaryResetsAt.Equal(modelReset) {
		t.Fatalf("seven_day_opus = %#v", limits)
	}
	// Overage has no slot and changes nothing.
	if got := c.claudeRateLimits(claudeLimitMeta("overage", 0.5, weekly), now); got.PrimaryUsedPercent != 25 || got.SecondaryUsedPercent != 90 {
		t.Fatalf("overage = %#v", got)
	}
	// Once the session window has refilled its old level is no longer shown.
	if got := c.claudeRateLimits(claudeLimitMeta("overage", 0.5, weekly), session.Add(time.Minute)); got.PrimaryUsedPercent != -1 || got.SecondaryUsedPercent != 90 {
		t.Fatalf("after session reset = %#v", got)
	}
	// A status-only update (no utilization) for a later window leaves that
	// window's level unknown instead of carrying the previous one forward.
	next := session.Add(5 * time.Hour)
	statusOnly := map[string]any{"_claude/rateLimit": map[string]any{"rateLimitType": "five_hour", "status": "allowed", "resetsAt": float64(next.Unix())}}
	if got := c.claudeRateLimits(statusOnly, session.Add(time.Minute)); got.PrimaryUsedPercent != -1 || got.SecondaryUsedPercent != 90 {
		t.Fatalf("status-only new window = %#v", got)
	}
	// The same reset without utilization keeps the known level.
	sameWindow := &conversation{}
	sameWindow.claudeRateLimits(claudeLimitMeta("five_hour", 0.85, session), now)
	statusSame := map[string]any{"_claude/rateLimit": map[string]any{"rateLimitType": "five_hour", "status": "allowed_warning", "resetsAt": float64(session.Unix())}}
	if got := sameWindow.claudeRateLimits(statusSame, now); got == nil || got.PrimaryUsedPercent != 85 {
		t.Fatalf("status-only same window = %#v", got)
	}
	// When it was the only slot, the quota position is reported as unknown so
	// the stored level is replaced instead of left standing.
	alone := &conversation{}
	alone.claudeRateLimits(claudeLimitMeta("five_hour", 0.85, session), now)
	if got := alone.claudeRateLimits(statusOnly, session.Add(time.Minute)); !allUnknown(got) {
		t.Fatalf("status-only new window, only slot = %#v, want all unknown", got)
	}
	if got := (&conversation{}).claudeRateLimits(claudeLimitMeta("overage", 0.5, weekly), now); got != nil {
		t.Fatalf("only an unslotted window = %#v, want nil", got)
	}
}

func percentIs(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func allUnknown(l *ports.ChatRateLimits) bool {
	return l != nil && l.PrimaryUsedPercent == -1 && l.SecondaryUsedPercent == -1
}

func decodeClaudeMeta(t *testing.T, raw string) map[string]any {
	t.Helper()
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

// The current Claude SDK sends no top-level utilization; the level lives in
// unifiedWindows. This is the exact payload a live chat-host journaled.
func TestClaudeRateLimitsReadUnifiedWindows(t *testing.T) {
	meta := decodeClaudeMeta(t, `{"_claude/rateLimit":{"status":"allowed","resetsAt":1795845600,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"out_of_credits","isUsingOverage":false,"unifiedWindows":{"seven_day":{"utilization":0.29,"resetsAt":1795845600,"status":"allowed"}}}}`)
	reset := time.Unix(1795845600, 0).UTC()
	now := reset.Add(-48 * time.Hour)
	c := &conversation{}
	limits := c.claudeRateLimits(meta, now)
	if limits == nil || !percentIs(limits.SecondaryUsedPercent, 29) || !limits.SecondaryResetsAt.Equal(reset) || limits.PrimaryUsedPercent != -1 {
		t.Fatalf("unified seven_day = %#v", limits)
	}
	// After the window has refilled its level is unknown, and that is reported
	// so the stored 29% does not outlive the window.
	if got := c.claudeRateLimits(meta, reset.Add(time.Second)); !allUnknown(got) {
		t.Fatalf("unified reading past its reset = %#v, want all unknown", got)
	}
}

func TestClaudeRateLimitsUnifiedSessionAndWeeklyWindows(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	session := now.Add(2 * time.Hour)
	weekly := now.Add(48 * time.Hour)
	model := now.Add(24 * time.Hour)
	meta := map[string]any{"_claude/rateLimit": map[string]any{
		"status": "allowed", "rateLimitType": "five_hour", "resetsAt": float64(session.Unix()),
		"unifiedWindows": map[string]any{
			"five_hour":      map[string]any{"utilization": 0.4, "resetsAt": float64(session.Unix()), "status": "allowed"},
			"seven_day":      map[string]any{"utilization": 0.29, "resetsAt": float64(weekly.Unix()), "status": "allowed"},
			"seven_day_opus": map[string]any{"utilization": 0.61, "resetsAt": float64(model.Unix()), "status": "allowed"},
			"overage":        map[string]any{"utilization": 0.99},
			"not_a_window":   "ignored",
		},
	}}
	c := &conversation{}
	limits := c.claudeRateLimits(meta, now)
	if limits == nil || !percentIs(limits.PrimaryUsedPercent, 40) || !limits.PrimaryResetsAt.Equal(session) {
		t.Fatalf("five_hour slot = %#v", limits)
	}
	// The tighter weekly window wins the weekly slot.
	if !percentIs(limits.SecondaryUsedPercent, 61) || !limits.SecondaryResetsAt.Equal(model) {
		t.Fatalf("weekly slot = %#v", limits)
	}
	// Once the session window has refilled only the weekly level remains.
	if got := c.claudeRateLimits(map[string]any{}, session.Add(time.Minute)); got != nil {
		t.Fatalf("meta without a rate limit = %#v, want nil", got)
	}
	afterSession := c.claudeRateLimits(meta, session.Add(time.Minute))
	if afterSession == nil || afterSession.PrimaryUsedPercent != -1 || !percentIs(afterSession.SecondaryUsedPercent, 61) {
		t.Fatalf("after session reset = %#v", afterSession)
	}
}

// A unified window wins over the same window's top-level fields, and a
// top-level window absent from unifiedWindows is still read (legacy shape).
func TestClaudeRateLimitsMixedUnifiedAndTopLevel(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	session := now.Add(2 * time.Hour)
	weekly := now.Add(48 * time.Hour)
	same := map[string]any{"_claude/rateLimit": map[string]any{
		"rateLimitType": "seven_day", "utilization": 0.85, "resetsAt": float64(weekly.Unix()),
		"unifiedWindows": map[string]any{"seven_day": map[string]any{"utilization": 0.29, "resetsAt": float64(weekly.Unix())}},
	}}
	if got := (&conversation{}).claudeRateLimits(same, now); got == nil || !percentIs(got.SecondaryUsedPercent, 29) {
		t.Fatalf("unified wins = %#v", got)
	}
	other := map[string]any{"_claude/rateLimit": map[string]any{
		"rateLimitType": "five_hour", "utilization": 0.5, "resetsAt": float64(session.Unix()),
		"unifiedWindows": map[string]any{"seven_day": map[string]any{"utilization": 0.29, "resetsAt": float64(weekly.Unix())}},
	}}
	got := (&conversation{}).claudeRateLimits(other, now)
	if got == nil || !percentIs(got.PrimaryUsedPercent, 50) || !percentIs(got.SecondaryUsedPercent, 29) {
		t.Fatalf("top-level five_hour plus unified seven_day = %#v", got)
	}
	// The legacy shape alone still reads.
	if legacy := (&conversation{}).claudeRateLimits(claudeLimitMeta("seven_day", 0.85, weekly), now); legacy == nil || !percentIs(legacy.SecondaryUsedPercent, 85) {
		t.Fatalf("legacy = %#v", legacy)
	}
	// A unified window without utilization is unknown, not zero.
	unknown := map[string]any{"_claude/rateLimit": map[string]any{
		"rateLimitType": "seven_day", "resetsAt": float64(weekly.Unix()),
		"unifiedWindows": map[string]any{"seven_day": map[string]any{"resetsAt": float64(weekly.Unix()), "status": "allowed"}},
	}}
	if got := (&conversation{}).claudeRateLimits(unknown, now); !allUnknown(got) {
		t.Fatalf("unified window without utilization = %#v, want all unknown", got)
	}
	// A malformed unified entry does not hide a valid top-level reading.
	malformed := map[string]any{"_claude/rateLimit": map[string]any{
		"rateLimitType": "seven_day", "utilization": 0.85, "resetsAt": float64(weekly.Unix()),
		"unifiedWindows": map[string]any{"seven_day": nil},
	}}
	if got := (&conversation{}).claudeRateLimits(malformed, now); got == nil || !percentIs(got.SecondaryUsedPercent, 85) {
		t.Fatalf("malformed unified entry = %#v, want the top-level 85", got)
	}
}

// Two equally tight weekly windows resolve to the one that refills first,
// whatever the map order.
func TestClaudeRateLimitsEqualWeeklyWindowsPreferEarlierReset(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	early, late := now.Add(24*time.Hour), now.Add(72*time.Hour)
	meta := map[string]any{"_claude/rateLimit": map[string]any{"unifiedWindows": map[string]any{
		"seven_day":        map[string]any{"utilization": 0.5, "resetsAt": float64(late.Unix())},
		"seven_day_opus":   map[string]any{"utilization": 0.5, "resetsAt": float64(early.Unix())},
		"seven_day_sonnet": map[string]any{"utilization": 0.5, "resetsAt": float64(late.Unix())},
	}}}
	for range 20 {
		got := (&conversation{}).claudeRateLimits(meta, now)
		if got == nil || !percentIs(got.SecondaryUsedPercent, 50) || !got.SecondaryResetsAt.Equal(early) {
			t.Fatalf("equal weekly windows = %#v, want 50 resetting at %v", got, early)
		}
	}
}
