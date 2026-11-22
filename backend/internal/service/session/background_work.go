package session

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// deriveBackgroundWork reports the work a live session started itself and is
// waiting on. It is a read-time projection: persisted activity is unchanged,
// so lifecycle delivery (review comments, CI nudges) still reaches a session
// whose turn ended while that work runs.
func deriveBackgroundWork(
	rec domain.SessionRecord,
	runs []domain.CurrentHeadReviewRun,
	chatItems []domain.BackgroundWorkItem,
	now time.Time,
) *domain.BackgroundWork {
	if rec.IsTerminated || rec.Activity.State == domain.ActivityExited {
		return nil
	}
	items := make([]domain.BackgroundWorkItem, 0)
	for _, task := range domain.PendingAgentBackgroundTasks(rec, now) {
		items = append(items, domain.BackgroundWorkItem{
			Kind: domain.BackgroundWorkAgentTask, ID: task.ID, Type: task.Type, Description: task.Description,
		})
	}
	items = append(items, chatItems...)
	// AO's own review pass is authoritative and bounded by the reviewer's own
	// lifecycle. Only a pass whose result AO delivers back to this session is
	// something the session waits on.
	if rec.AutoInjectReview {
		for _, run := range runs {
			if run.Status == domain.ReviewRunRunning {
				items = append(items, domain.BackgroundWorkItem{
					Kind: domain.BackgroundWorkReview, ID: run.ID, Type: string(run.Harness),
				})
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	return &domain.BackgroundWork{Pending: true, Items: items}
}

// withBackgroundActivity reports a session idle only in its turn as busy while
// it waits on its own background work. Needs-input, blocked and exited states
// are left alone: they say more than "waiting".
func withBackgroundActivity(rec domain.SessionRecord, work *domain.BackgroundWork) domain.SessionRecord {
	if work != nil && work.Pending && rec.Activity.State == domain.ActivityIdle {
		rec.Activity.State = domain.ActivityActive
	}
	return rec
}
