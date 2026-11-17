package acp

import (
	"testing"
	"time"
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
	// When it was the only slot, the quota position is unknown.
	alone := &conversation{}
	alone.claudeRateLimits(claudeLimitMeta("five_hour", 0.85, session), now)
	if got := alone.claudeRateLimits(statusOnly, session.Add(time.Minute)); got != nil {
		t.Fatalf("status-only new window, only slot = %#v, want nil", got)
	}
	if got := (&conversation{}).claudeRateLimits(claudeLimitMeta("overage", 0.5, weekly), now); got != nil {
		t.Fatalf("only an unslotted window = %#v, want nil", got)
	}
}
