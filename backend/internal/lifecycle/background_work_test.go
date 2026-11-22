package lifecycle

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A parent Stop that leaves background shells running records them as facts
// for the read model but does not rewrite persisted activity: delivery (review
// feedback the worker is waiting for) must still see an idle agent.
func TestClaudeStopRecordsBackgroundTasksWithoutChangingActivity(t *testing.T) {
	store := newFakeStore()
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	store.sessions["ao-1"] = domain.SessionRecord{
		ID: "ao-1", Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI,
		Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: start},
		Metadata: domain.SessionMetadata{RuntimeLaunchID: "launch-1"},
	}
	m := New(store, nil)
	stop := func(second int, tasks *[]domain.BackgroundTask, launch string) {
		t.Helper()
		if err := m.ApplyActivitySignal(context.Background(), "ao-1", ports.ActivitySignal{
			Valid: true, State: domain.ActivityIdle, Event: "stop",
			BackgroundTasks: tasks, LaunchID: launch, AgentSessionID: "native-1",
			Timestamp: start.Add(time.Duration(second) * time.Second),
		}); err != nil {
			t.Fatalf("stop: %v", err)
		}
	}
	running := []domain.BackgroundTask{{ID: "bash-1", Type: "local_bash", Description: "ao review trigger --wait"}}
	stop(1, &running, "launch-1")
	rec := store.sessions["ao-1"]
	if rec.Activity.State != domain.ActivityIdle {
		t.Fatalf("persisted activity = %q, want idle", rec.Activity.State)
	}
	got := domain.PendingAgentBackgroundTasks(rec, start.Add(time.Minute))
	if len(got) != 1 || got[0].ID != "bash-1" || got[0].Type != "local_bash" {
		t.Fatalf("pending tasks = %+v, want bash-1", got)
	}

	// A delayed older snapshot cannot replace a newer one.
	empty := []domain.BackgroundTask{}
	stop(0, &empty, "launch-1")
	if got := domain.PendingAgentBackgroundTasks(store.sessions["ao-1"], start.Add(time.Minute)); len(got) != 1 {
		t.Fatalf("delayed snapshot replaced newer: %+v", got)
	}
	// A Stop without a snapshot (older Claude) keeps what is known.
	stop(2, nil, "launch-1")
	if got := domain.PendingAgentBackgroundTasks(store.sessions["ao-1"], start.Add(time.Minute)); len(got) != 1 {
		t.Fatalf("snapshot-less stop cleared tasks: %+v", got)
	}
	// The task finished: Claude's next turn ends with an empty snapshot.
	stop(3, &empty, "launch-1")
	if got := domain.PendingAgentBackgroundTasks(store.sessions["ao-1"], start.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("pending tasks after completion = %+v, want none", got)
	}
}

func TestPendingAgentBackgroundTasksIgnoresOtherLaunchAndStaleSnapshots(t *testing.T) {
	store := newFakeStore()
	start := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	store.sessions["ao-1"] = domain.SessionRecord{
		ID: "ao-1", Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI,
		Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: start},
		Metadata: domain.SessionMetadata{RuntimeLaunchID: "launch-1"},
	}
	m := New(store, nil)
	running := []domain.BackgroundTask{{ID: "monitor-1", Type: "monitor"}}
	if err := m.ApplyActivitySignal(context.Background(), "ao-1", ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop", BackgroundTasks: &running,
		LaunchID: "launch-1", Timestamp: start,
	}); err != nil {
		t.Fatal(err)
	}
	rec := store.sessions["ao-1"]
	if got := domain.PendingAgentBackgroundTasks(rec, start.Add(domain.AgentBackgroundTaskCeiling)); len(got) != 0 {
		t.Fatalf("snapshot past the ceiling still pending: %+v", got)
	}
	rec.Metadata.RuntimeLaunchID = "launch-2"
	if got := domain.PendingAgentBackgroundTasks(rec, start.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("previous launch's tasks still pending: %+v", got)
	}
}

func idleMergedSession(id domain.SessionID) domain.SessionRecord {
	rec := working(id)
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now().Add(-time.Hour)}
	rec.TerminateOnPRMerge = true
	return rec
}

// The fleet master sends the next task as soon as it sees the PR merged. The
// worker is still idle until its prompt-submit hook lands; tearing it down in
// that window drops the message silently.
func TestPRObservation_MergedDefersTeardownRightAfterASend(t *testing.T) {
	m, st, _ := newManager()
	terminator := &fakeCompletionTerminator{}
	m.SetCompletionTerminator(terminator)
	rec := idleMergedSession("mer-1")
	rec.Metadata.LatestInteractionAt = m.clock().Add(-10 * time.Second)
	st.sessions["mer-1"] = rec
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: true}}

	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true}); err != nil {
		t.Fatal(err)
	}
	if terminator.calls != 0 || st.sessions["mer-1"].IsTerminated {
		t.Fatalf("merged teardown ran right after a send (calls=%d)", terminator.calls)
	}
}

func TestPRObservation_MergedDefersTeardownWhileBackgroundWorkRuns(t *testing.T) {
	m, st, _ := newManager()
	terminator := &fakeCompletionTerminator{}
	m.SetCompletionTerminator(terminator)
	rec := idleMergedSession("mer-1")
	rec.Harness = domain.HarnessClaudeCode
	rec.Mode = domain.SessionModeTUI
	rec.Metadata.RuntimeLaunchID = "launch-1"
	rec.Metadata.ClaudeActivityFacts = `{"launchId":"launch-1","backgroundTasks":[{"id":"bash-1","type":"local_bash"}],"backgroundAt":` +
		strconv.FormatInt(m.clock().Add(-time.Minute).UnixNano(), 10) + `}`
	st.sessions["mer-1"] = rec
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: true}}

	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true}); err != nil {
		t.Fatal(err)
	}
	if terminator.calls != 0 {
		t.Fatalf("merged teardown killed a worker waiting on background work (calls=%d)", terminator.calls)
	}
}

func TestPRObservation_MergedIdleWorkerWithoutPendingWorkStillTerminates(t *testing.T) {
	m, st, _ := newManager()
	terminator := &fakeCompletionTerminator{}
	m.SetCompletionTerminator(terminator)
	rec := idleMergedSession("mer-1")
	rec.Metadata.LatestInteractionAt = m.clock().Add(-time.Hour)
	st.sessions["mer-1"] = rec
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: true}}

	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true}); err != nil {
		t.Fatal(err)
	}
	if terminator.calls != 1 {
		t.Fatalf("terminator calls = %d, want 1 for a finished merged worker", terminator.calls)
	}
}

// A message a turn already picked up (activity changed after it, e.g. the
// spawn prompt) does not hold a finished merged worker alive.
func TestPRObservation_MergedHandledMessageDoesNotDeferTeardown(t *testing.T) {
	m, st, _ := newManager()
	terminator := &fakeCompletionTerminator{}
	m.SetCompletionTerminator(terminator)
	rec := idleMergedSession("mer-1")
	rec.Metadata.LatestInteractionAt = m.clock().Add(-30 * time.Second)
	rec.Activity.LastActivityAt = m.clock().Add(-10 * time.Second)
	st.sessions["mer-1"] = rec
	st.prs["mer-1"] = []domain.PullRequest{{URL: "pr1", Merged: true}}

	if err := m.ApplyPRObservation(ctx, "mer-1", ports.PRObservation{Fetched: true, URL: "pr1", Merged: true}); err != nil {
		t.Fatal(err)
	}
	if terminator.calls != 1 {
		t.Fatalf("terminator calls = %d, want 1 once the message's turn ran", terminator.calls)
	}
}
