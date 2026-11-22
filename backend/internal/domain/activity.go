package domain

import (
	"encoding/json"
	"time"
)

// ActivityState is how busy the agent is, reported via the agent's CLI hook
// callbacks, not inferred from transcript/JSONL
type ActivityState string

// Activity states. WaitingInput and Blocked are sticky (see IsSticky).
//
// WaitingInput and Blocked both mean "paused on the user" but demand opposite
// automation: waiting_input is an agent at an empty prompt awaiting its next
// INSTRUCTION (safe to message or nudge), while blocked is an agent stopped on
// a pending DECISION — a tool-permission or approval dialog — where a stray
// keystroke could answer the dialog on the user's behalf. Automated senders
// must never inject input into a blocked session. (Not to be confused with the
// PR-stack Blocked flag in the status read model; blocked here predates it —
// the state existed in the original activity model and returns with the
// permission-prompt producers.)
const (
	ActivityActive       ActivityState = "active"
	ActivityIdle         ActivityState = "idle"
	ActivityWaitingInput ActivityState = "waiting_input"
	ActivityBlocked      ActivityState = "blocked"
	ActivityExited       ActivityState = "exited"
)

// IsSticky reports whether an activity state must NOT be aged/demoted by the
// passage of time (a paused agent is still paused until a new signal says so).
func (a ActivityState) IsSticky() bool {
	return a == ActivityWaitingInput || a == ActivityBlocked
}

// NeedsInput reports whether the agent is paused on the user — waiting for the
// next instruction (waiting_input) or blocked on a decision (blocked). Both
// render as the needs_input session status. Distinct from IsSticky: stickiness
// is about time-demotion, NeedsInput about the user being the unblocker.
func (a ActivityState) NeedsInput() bool {
	return a == ActivityWaitingInput || a == ActivityBlocked
}

// Activity captures the persisted activity reading: the state and when it was
// last observed.
type Activity struct {
	State          ActivityState `json:"state"`
	LastActivityAt time.Time     `json:"lastActivityAt"`
}

// TurnFailureCandidate is a live terminal session in a turn (active or
// waiting_input) with a native transcript path: the narrow read the failed-turn
// observer polls instead of every session row.
type TurnFailureCandidate struct {
	ID                   SessionID
	Harness              AgentHarness
	Activity             Activity
	Revision             int64
	RuntimeLaunchID      string
	NativeTranscriptPath string
	// LatestUserPromptAt is the newest human prompt submission. A prompt
	// submitted while the session already read active leaves
	// Activity.LastActivityAt where the earlier turn started.
	LatestUserPromptAt time.Time
}

// BackgroundTask is one unit of work an agent started itself and that outlives
// the turn that started it: a background shell, a Monitor watch, a background
// agent. The provider reports these in its own task registry (Claude's Stop
// hook background_tasks snapshot); AO only mirrors them so a parent that ended
// its turn while waiting on them is not mistaken for a finished, idle agent.
type BackgroundTask struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// Background-work kinds reported in the session read model.
const (
	// BackgroundWorkAgentTask is a provider-native background task (shell,
	// monitor, ...) the agent started and has not seen finish yet.
	BackgroundWorkAgentTask = "agent_task"
	// BackgroundWorkProvider is unnamed background work a Chat provider still
	// reports as live after its turn ended.
	BackgroundWorkProvider = "provider"
	// BackgroundWorkReview is an AO review pass running on the session's PR
	// head; its result is delivered back to the session.
	BackgroundWorkReview = "review"
)

// BackgroundWorkItem names one thing a session is waiting on.
type BackgroundWorkItem struct {
	Kind        string `json:"kind" enum:"agent_task,provider,review"`
	ID          string `json:"id,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

// BackgroundWork reports that a session is waiting on work it started itself.
// A session in this state is not idle: its next turn starts when the work
// finishes, without anyone sending it a message.
type BackgroundWork struct {
	Pending bool                 `json:"pending"`
	Items   []BackgroundWorkItem `json:"items"`
}

// MaxBackgroundTaskDescription bounds a mirrored task description.
const MaxBackgroundTaskDescription = 200

// ParseActivityFactsBackground reads the background-task snapshot lifecycle
// stores in a session's native activity-facts blob, with the launch it belongs
// to and when it was observed. A damaged or absent blob reports nothing.
func ParseActivityFactsBackground(raw string) (tasks []BackgroundTask, launchID string, at time.Time) {
	if raw == "" {
		return nil, "", time.Time{}
	}
	var facts struct {
		LaunchID        string           `json:"launchId"`
		BackgroundTasks []BackgroundTask `json:"backgroundTasks"`
		BackgroundAt    int64            `json:"backgroundAt"`
	}
	if json.Unmarshal([]byte(raw), &facts) != nil || facts.BackgroundAt <= 0 {
		return nil, "", time.Time{}
	}
	return facts.BackgroundTasks, facts.LaunchID, time.Unix(0, facts.BackgroundAt)
}

// AgentBackgroundTaskCeiling bounds how long a provider task snapshot can keep
// an otherwise idle session reported as waiting. Claude reports finished
// background work by starting a new turn, whose Stop refreshes the snapshot,
// so a live wait is re-confirmed on its own. A task the agent abandoned (a dev
// server left running) produces no such turn; past this age the snapshot no
// longer claims the session is waiting, so a finished worker still settles.
const AgentBackgroundTaskCeiling = time.Hour

// PendingAgentBackgroundTasks returns the current process generation's
// background-task snapshot while it is fresh enough to describe a live wait.
func PendingAgentBackgroundTasks(rec SessionRecord, now time.Time) []BackgroundTask {
	if rec.Harness != HarnessClaudeCode || NormalizeSessionMode(rec.Mode) != SessionModeTUI {
		return nil
	}
	tasks, launchID, at := ParseActivityFactsBackground(rec.Metadata.ClaudeActivityFacts)
	if launchID == "" || launchID != rec.Metadata.RuntimeLaunchID || now.Sub(at) >= AgentBackgroundTaskCeiling {
		return nil
	}
	return tasks
}
