package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
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

func snapshotRateLimits(t *testing.T, svc *chatsvc.Service, id domain.SessionID) (*domain.ConversationRateLimits, *domain.ConversationRateLimits) {
	t.Helper()
	ctx := context.Background()
	full, err := svc.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("Snapshot(%s): %v", id, err)
	}
	page, err := svc.SnapshotPage(ctx, id, 0, 0, 50)
	if err != nil {
		t.Fatalf("SnapshotPage(%s): %v", id, err)
	}
	if full.Conversation.RateLimits != full.RateLimits || page.Conversation.RateLimits != page.RateLimits {
		t.Fatalf("snapshot of %s carries a second quota copy: %#v / %#v", id, full.Conversation.RateLimits, page.Conversation.RateLimits)
	}
	return full.RateLimits, page.RateLimits
}

func awaitServedRateLimits(t *testing.T, svc *chatsvc.Service, id domain.SessionID, pred func(*domain.ConversationRateLimits) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var full, page *domain.ConversationRateLimits
	for time.Now().Before(deadline) {
		full, page = snapshotRateLimits(t, svc, id)
		if pred(full) && pred(page) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("served rate limits of %s = %#v / %#v", id, full, page)
}

func seedClaudeWorker(t *testing.T, h *harness) domain.SessionID {
	t.Helper()
	ctx := context.Background()
	now := h.now()
	record, err := h.st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject,
		Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateConversation(ctx, uuid.NewString(), domain.ConversationScopeSession, testProject, record.ID, now); err != nil {
		t.Fatal(err)
	}
	return record.ID
}

// The composer badge is the session's own snapshot. A session without a
// reading of its own never shows another session's, and a session whose agent
// is not running never shows the reading its last process left behind.
func TestSnapshotServesOnlyTheSessionsOwnLiveRateLimits(t *testing.T) {
	h := newHarnessForHarness(t, domain.HarnessClaudeCode)
	weekly := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventRateLimits, RateLimits: &ports.ChatRateLimits{
		PrimaryUsedPercent: -1, SecondaryUsedPercent: 85, SecondaryResetsAt: weekly,
	}})
	awaitServedRateLimits(t, h.svc, testSession, func(l *domain.ConversationRateLimits) bool {
		return l != nil && l.SecondaryUsedPercent == 85
	})

	fresh := seedClaudeWorker(t, h)
	if full, page := snapshotRateLimits(t, h.svc, fresh); full != nil || page != nil {
		t.Fatalf("worker without stored limits shows %#v / %#v", full, page)
	}

	// A worker row still holding a reading from a process that is gone.
	stale := seedClaudeWorker(t, h)
	conversation, err := h.st.ConversationForSession(context.Background(), stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.RecordRateLimits(context.Background(), conversation.ID, domain.ConversationRateLimits{
		PrimaryUsedPercent: -1, SecondaryUsedPercent: 94, SecondaryResetsAt: weekly,
	}); err != nil {
		t.Fatal(err)
	}
	if full, page := snapshotRateLimits(t, h.svc, stale); full != nil || page != nil {
		t.Fatalf("worker without a running agent shows its stale %#v / %#v", full, page)
	}
	// The live session still shows its own reading.
	if full, _ := snapshotRateLimits(t, h.svc, testSession); full == nil || full.SecondaryUsedPercent != 85 {
		t.Fatalf("live session lost its own reading: %#v", full)
	}
}

func TestSnapshotHidesRateLimitsOnceTheControllerStops(t *testing.T) {
	h := newHarnessForHarness(t, domain.HarnessClaudeCode)
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventRateLimits, RateLimits: &ports.ChatRateLimits{
		PrimaryUsedPercent: -1, SecondaryUsedPercent: 85, SecondaryResetsAt: time.Now().Add(48 * time.Hour),
	}})
	awaitServedRateLimits(t, h.svc, testSession, func(l *domain.ConversationRateLimits) bool { return l != nil })
	if err := h.svc.Stop(context.Background(), testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	awaitServedRateLimits(t, h.svc, testSession, func(l *domain.ConversationRateLimits) bool { return l == nil })
}

type failingRateLimitClearStore struct{ chatsvc.Store }

func (s failingRateLimitClearStore) RecordRateLimits(ctx context.Context, id string, limits domain.ConversationRateLimits) error {
	if limits.PrimaryUsedPercent < 0 && limits.SecondaryUsedPercent < 0 {
		return errors.New("clear refused")
	}
	return s.Store.RecordRateLimits(ctx, id, limits)
}

// A controller that could not clear what an earlier process left does not
// present that row as its own; its first reading does.
func TestSnapshotWithholdsRateLimitsUntilTheControllerOwnsThem(t *testing.T) {
	h := newHarnessWithConversationAndStoreForHarness(t, nil, func(st *sqlite.Store) chatsvc.Store {
		return failingRateLimitClearStore{Store: st}
	}, domain.HarnessClaudeCode)
	weekly := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	if err := h.st.RecordRateLimits(context.Background(), h.ctrl.ConversationID(), domain.ConversationRateLimits{
		PrimaryUsedPercent: -1, SecondaryUsedPercent: 85, SecondaryResetsAt: weekly,
	}); err != nil {
		t.Fatal(err)
	}
	if full, page := snapshotRateLimits(t, h.svc, testSession); full != nil || page != nil {
		t.Fatalf("uncleared earlier reading served: %#v / %#v", full, page)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventRateLimits, RateLimits: &ports.ChatRateLimits{
		PrimaryUsedPercent: -1, SecondaryUsedPercent: 29, SecondaryResetsAt: weekly,
	}})
	awaitServedRateLimits(t, h.svc, testSession, func(l *domain.ConversationRateLimits) bool {
		return l != nil && l.SecondaryUsedPercent == 29
	})
}
