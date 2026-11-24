package chat_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// A quota reading belongs to the provider process that sent it, which may have
// been signed in to another account. A controller opened on a new process must
// not present the previous process's reading as current: the meter stays empty
// until the new process reports its own.
func TestRestartedControllerDoesNotSurfacePreviousProcessRateLimits(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	record, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("read seeded session: found=%v, err=%v", found, err)
	}
	record.Harness = domain.HarnessClaudeCode
	if err := st.UpdateSession(ctx, record); err != nil {
		t.Fatalf("set seeded harness: %v", err)
	}
	first, second := newFakeConversation(), newFakeConversation()
	var sequence atomic.Int64
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Sessions: st, Reader: fullSnapshotReader(st),
		Drivers: fakeRegistry{driver: &sequenceDriver{conversations: []ports.ChatConversation{first, second}}},
		Log:     slog.New(slog.DiscardHandler),
		NewID:   func() string { return fmt.Sprintf("rate-limit-restart-%d", sequence.Add(1)) },
	})
	t.Cleanup(func() { svc.StopAll(ctx) })
	cfg := chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessClaudeCode,
		WorkspacePath: t.TempDir(),
	}
	controller, err := svc.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	conversationID := controller.ConversationID()
	weekly := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	awaitLimits := func(what string, pred func(*domain.ConversationRateLimits) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		var last *domain.ConversationRateLimits
		for time.Now().Before(deadline) {
			snapshot, err := st.LoadConversationSnapshot(ctx, conversationID)
			if err != nil {
				t.Fatalf("load snapshot: %v", err)
			}
			last = snapshot.Conversation.RateLimits
			if pred(last) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: rate limits = %#v", what, last)
	}

	first.emit(ports.ChatEvent{Kind: ports.ChatEventRateLimits, RateLimits: &ports.ChatRateLimits{
		PrimaryUsedPercent: -1, SecondaryUsedPercent: 85, SecondaryResetsAt: weekly,
	}})
	awaitLimits("first process reading", func(l *domain.ConversationRateLimits) bool {
		return l != nil && l.SecondaryUsedPercent == 85
	})

	if err := svc.Stop(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	cfg.ProviderConversationID = "thread-1"
	restarted, err := svc.Start(ctx, cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if restarted.ConversationID() != conversationID {
		t.Fatalf("restart opened conversation %q, want %q", restarted.ConversationID(), conversationID)
	}
	// The previous process's reading is still within its window, yet it is
	// no longer shown.
	awaitLimits("after restart", func(l *domain.ConversationRateLimits) bool { return l == nil })

	second.emit(ports.ChatEvent{Kind: ports.ChatEventRateLimits, RateLimits: &ports.ChatRateLimits{
		PrimaryUsedPercent: -1, SecondaryUsedPercent: 29, SecondaryResetsAt: weekly,
	}})
	awaitLimits("second process reading", func(l *domain.ConversationRateLimits) bool {
		return l != nil && l.SecondaryUsedPercent == 29 && l.SecondaryResetsAt.Equal(weekly)
	})
}
