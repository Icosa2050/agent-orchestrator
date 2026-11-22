package session

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/pkg/contract"
)

func idleWorker() domain.SessionRecord {
	return domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer", Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI,
		Activity:         domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now().Add(-time.Minute)},
		FirstSignalAt:    time.Now().Add(-time.Hour),
		AutoInjectReview: true,
	}
}

// A worker that triggered a review and waits for its delivered result is not
// idle while the pass runs, and is idle again once it completes.
func TestReadModelReviewPendingIsNotIdle(t *testing.T) {
	st := newFakeStore()
	st.sessions["mer-1"] = idleWorker()
	st.pr["mer-1"] = domain.PRFacts{URL: "pr1", HeadSHA: "head1"}
	st.reviewRuns["mer-1"] = []domain.CurrentHeadReviewRun{{
		SessionID: "mer-1", PRURL: "pr1", ID: "run-1", Harness: "claude-code", Status: domain.ReviewRunRunning,
	}}
	svc := &Service{store: st}

	got, err := svc.Get(context.Background(), "mer-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BackgroundWork == nil || !got.BackgroundWork.Pending || len(got.BackgroundWork.Items) != 1 ||
		got.BackgroundWork.Items[0].Kind != domain.BackgroundWorkReview || got.BackgroundWork.Items[0].ID != "run-1" {
		t.Fatalf("backgroundWork = %+v, want the running review", got.BackgroundWork)
	}
	if got.Activity.State != domain.ActivityActive || got.Status != domain.StatusWorking {
		t.Fatalf("activity=%q status=%q, want active/working while the review runs", got.Activity.State, got.Status)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Activity       struct{ State string } `json:"activity"`
		BackgroundWork *struct {
			Pending bool `json:"pending"`
			Items   []struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"items"`
		} `json:"backgroundWork"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Activity.State != "active" || wire.BackgroundWork == nil || !wire.BackgroundWork.Pending ||
		wire.BackgroundWork.Items[0].Kind != "review" {
		t.Fatalf("wire JSON = %s", encoded)
	}

	st.reviewRuns["mer-1"] = []domain.CurrentHeadReviewRun{{
		SessionID: "mer-1", PRURL: "pr1", ID: "run-1", Harness: "claude-code", Status: domain.ReviewRunComplete,
	}}
	got, err = svc.Get(context.Background(), "mer-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BackgroundWork != nil || got.Activity.State != domain.ActivityIdle {
		t.Fatalf("after completion: backgroundWork=%+v activity=%q, want none/idle", got.BackgroundWork, got.Activity.State)
	}
	encoded, _ = json.Marshal(got)
	var raw map[string]any
	_ = json.Unmarshal(encoded, &raw)
	if _, present := raw["backgroundWork"]; present {
		t.Fatalf("backgroundWork must be omitted when nothing is pending: %s", encoded)
	}
}

// A pass whose result is not delivered back is not something the session
// waits on.
func TestReadModelReviewWithoutDeliveryIsNotBackgroundWork(t *testing.T) {
	rec := idleWorker()
	rec.AutoInjectReview = false
	runs := []domain.CurrentHeadReviewRun{{ID: "run-1", Status: domain.ReviewRunRunning}}
	if got := deriveBackgroundWork(rec, runs, nil, time.Now()); got != nil {
		t.Fatalf("backgroundWork = %+v, want none", got)
	}
}

func TestReadModelAgentBackgroundTasks(t *testing.T) {
	now := time.Now()
	rec := idleWorker()
	rec.Metadata.RuntimeLaunchID = "launch-1"
	rec.Metadata.ClaudeActivityFacts = `{"launchId":"launch-1","backgroundTasks":[{"id":"bash-1","type":"local_bash","description":"npm test"}],"backgroundAt":` +
		strconv.FormatInt(now.Add(-5*time.Minute).UnixNano(), 10) + `}`

	work := deriveBackgroundWork(rec, nil, nil, now)
	if work == nil || len(work.Items) != 1 || work.Items[0].Kind != domain.BackgroundWorkAgentTask ||
		work.Items[0].ID != "bash-1" || work.Items[0].Description != "npm test" {
		t.Fatalf("backgroundWork = %+v, want bash-1", work)
	}
	if got := withBackgroundActivity(rec, work).Activity.State; got != domain.ActivityActive {
		t.Fatalf("activity = %q, want active", got)
	}
	// Past the ceiling an abandoned task (a dev server) no longer holds it.
	if work := deriveBackgroundWork(rec, nil, nil, now.Add(domain.AgentBackgroundTaskCeiling)); work != nil {
		t.Fatalf("stale snapshot still pending: %+v", work)
	}
	// Terminated sessions wait on nothing.
	rec.IsTerminated = true
	if work := deriveBackgroundWork(rec, nil, nil, now); work != nil {
		t.Fatalf("terminated session reports background work: %+v", work)
	}
}

func TestReadModelChatProviderBackgroundWork(t *testing.T) {
	st := newFakeStore()
	rec := idleWorker()
	rec.Mode = domain.SessionModeChat
	st.sessions["mer-1"] = rec
	svc := &Service{store: st}
	svc.SetChatBackgroundWork(func(id domain.SessionID) []domain.BackgroundWorkItem {
		if id != "mer-1" {
			return nil
		}
		return []domain.BackgroundWorkItem{{Kind: domain.BackgroundWorkProvider, Type: "claude-code"}}
	})
	got, err := svc.Get(context.Background(), "mer-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BackgroundWork == nil || got.BackgroundWork.Items[0].Kind != domain.BackgroundWorkProvider ||
		got.Activity.State != domain.ActivityActive {
		t.Fatalf("backgroundWork=%+v activity=%q", got.BackgroundWork, got.Activity.State)
	}
}

// Needs-input outranks "waiting": the user is the unblocker.
func TestReadModelBackgroundWorkKeepsNeedsInput(t *testing.T) {
	rec := idleWorker()
	rec.Activity.State = domain.ActivityWaitingInput
	work := &domain.BackgroundWork{Pending: true, Items: []domain.BackgroundWorkItem{{Kind: domain.BackgroundWorkReview}}}
	if got := withBackgroundActivity(rec, work).Activity.State; got != domain.ActivityWaitingInput {
		t.Fatalf("activity = %q, want waiting_input", got)
	}
}

// Waiting on a review is not "busy on this PR": an idle worker whose head is
// being reviewed while CI fails reads Reviewing, never Fixing CI, even though
// its status and activity report the wait.
func TestReadModelBackgroundWorkDoesNotDriveKanbanBusyLabels(t *testing.T) {
	st := newFakeStore()
	rec := idleWorker()
	rec.AutoInjectCI = true
	rec.AutoReviewEnabled = true
	st.sessions["mer-1"] = rec
	st.pr["mer-1"] = domain.PRFacts{URL: "pr1", HeadSHA: "head1", CI: domain.CIFailing}
	st.reviewRuns["mer-1"] = []domain.CurrentHeadReviewRun{{
		SessionID: "mer-1", PRURL: "pr1", ID: "run-1", Harness: "claude-code", Status: domain.ReviewRunRunning,
	}}

	got, err := (&Service{store: st}).Get(context.Background(), "mer-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.BackgroundWork == nil || got.Activity.State != domain.ActivityActive {
		t.Fatalf("backgroundWork=%+v activity=%q, want the review wait reported", got.BackgroundWork, got.Activity.State)
	}
	if got.DisplayStatus != contract.DisplayReviewing {
		t.Fatalf("display status = %q, want %q (the worker is not fixing CI)", got.DisplayStatus, contract.DisplayReviewing)
	}
}
