package sessionmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

// A merged worker torn down by its completion teardown right after the paste
// never runs the message. The send must say so, not report success.
func TestSend_SessionTerminatedDuringDeliveryIsAnError(t *testing.T) {
	st := newFakeStore()
	st.sessions["s1"] = pastStartupGate(domain.SessionRecord{ID: "s1", Harness: "claude-code",
		Activity: domain.Activity{State: domain.ActivityIdle}})
	msg := &fakeMessenger{onSend: func(id domain.SessionID, _ string) {
		rec := st.sessions[id]
		rec.IsTerminated = true
		rec.Activity.State = domain.ActivityExited
		st.sessions[id] = rec
	}}
	m := newSendTestManager(t, signalingAgent{}, msg, st)

	err := m.Send(context.Background(), "s1", "next task after merge", nil)
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("Send error = %v, want ErrTerminated", err)
	}
	if len(msg.msgs) != 1 {
		t.Fatalf("pane writes = %d, want only the original paste (no Enter into a dead pane)", len(msg.msgs))
	}
}

func TestSend_AgentExitedDuringDeliveryIsAnError(t *testing.T) {
	st := newFakeStore()
	st.sessions["s1"] = pastStartupGate(domain.SessionRecord{ID: "s1", Harness: "claude-code",
		Activity: domain.Activity{State: domain.ActivityIdle}})
	msg := &fakeMessenger{onSend: func(id domain.SessionID, _ string) {
		rec := st.sessions[id]
		rec.Activity.State = domain.ActivityExited
		st.sessions[id] = rec
	}}
	m := newSendTestManager(t, signalingAgent{}, msg, st)

	if err := m.Send(context.Background(), "s1", "hello", nil); !errors.Is(err, ErrAgentExited) {
		t.Fatalf("Send error = %v, want ErrAgentExited", err)
	}
}

func TestEndedDuringDeliveryErr(t *testing.T) {
	cases := map[sessionguard.Outcome]error{
		sessionguard.SuppressedTerminated:   ErrTerminated,
		sessionguard.SuppressedExited:       ErrAgentExited,
		sessionguard.SuppressedNotFound:     ErrNotFound,
		sessionguard.SuppressedBusy:         nil,
		sessionguard.SuppressedAwaitingUser: nil,
		sessionguard.SuppressedInputGated:   nil,
	}
	for outcome, want := range cases {
		if got := endedDuringDeliveryErr(outcome); !errors.Is(got, want) || (want == nil && got != nil) {
			t.Fatalf("endedDuringDeliveryErr(%s) = %v, want %v", outcome, got, want)
		}
	}
}
