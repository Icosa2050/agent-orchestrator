package controllers

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The stored instant is sent as RFC 3339 and the countdown is taken when the
// response is built, so a reloaded conversation never shows a frozen reset.
func TestRateLimitsPayloadCountsDownFromTheStoredInstant(t *testing.T) {
	weekly := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	payload := rateLimitsPayload(&domain.ConversationRateLimits{
		PrimaryUsedPercent: 20, SecondaryUsedPercent: 84,
		PrimaryResetsAt: time.Now().Add(-time.Minute), SecondaryResetsAt: weekly,
	})
	if payload.SecondaryResetsAt != weekly.Format(time.RFC3339) {
		t.Fatalf("secondary resets at %q, want %q", payload.SecondaryResetsAt, weekly.Format(time.RFC3339))
	}
	if got := payload.SecondaryResetsInSeconds; got <= 47*3600 || got > 48*3600 {
		t.Fatalf("secondary resets in %d", got)
	}
	// A reset that has passed counts down to nothing.
	if payload.PrimaryResetsInSeconds != 0 {
		t.Fatalf("primary resets in %d, want 0", payload.PrimaryResetsInSeconds)
	}
	if empty := rateLimitsPayload(&domain.ConversationRateLimits{PrimaryUsedPercent: -1, SecondaryUsedPercent: -1}); empty.PrimaryResetsAt != "" || empty.SecondaryResetsAt != "" {
		t.Fatalf("unknown resets = %+v", empty)
	}
}
