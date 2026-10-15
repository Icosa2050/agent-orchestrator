-- name: InsertAutomation :exec
INSERT INTO automations (
    id, project_id, display_name, prompt, kind, harness, rrule_text,
    timezone, enabled, next_run_at, last_run_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAutomation :one
SELECT id, project_id, display_name, prompt, kind, harness, rrule_text,
    timezone, enabled, next_run_at, last_run_at, created_at, updated_at
FROM automations
WHERE id = ?;

-- name: ListAutomations :many
SELECT id, project_id, display_name, prompt, kind, harness, rrule_text,
    timezone, enabled, next_run_at, last_run_at, created_at, updated_at
FROM automations
WHERE (sqlc.arg(project_id) = '' OR project_id = sqlc.arg(project_id))
  AND (sqlc.arg(enabled_filter) < 0 OR enabled = sqlc.arg(enabled_filter))
ORDER BY created_at, id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountAutomations :one
SELECT COUNT(*)
FROM automations
WHERE (sqlc.arg(project_id) = '' OR project_id = sqlc.arg(project_id))
  AND (sqlc.arg(enabled_filter) < 0 OR enabled = sqlc.arg(enabled_filter));

-- name: UpdateAutomation :execrows
UPDATE automations
SET display_name = ?, prompt = ?, kind = ?, harness = ?, rrule_text = ?,
    timezone = ?, enabled = ?, next_run_at = ?, last_run_at = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteAutomation :execrows
DELETE FROM automations WHERE id = ?;

-- name: AdvanceAutomationSchedule :execrows
UPDATE automations
SET last_run_at = ?, next_run_at = ?, updated_at = ?
WHERE id = ? AND enabled = 1 AND next_run_at = ?;

-- name: ListDueAutomations :many
SELECT id, project_id, display_name, prompt, kind, harness, rrule_text,
    timezone, enabled, next_run_at, last_run_at, created_at, updated_at
FROM automations
WHERE enabled = 1 AND next_run_at <= ?
ORDER BY next_run_at, id
LIMIT ?;

-- name: InsertAutomationRun :execrows
INSERT INTO automation_runs (
    id, automation_id, scheduled_for, session_id, status, attempt_count,
    claimed_at, lease_expires_at, started_at, finished_at, error_message,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (automation_id, scheduled_for) DO NOTHING;

-- name: GetAutomationRun :one
SELECT id, automation_id, scheduled_for, session_id, status, attempt_count,
    claimed_at, lease_expires_at, started_at, finished_at, error_message,
    created_at, updated_at
FROM automation_runs
WHERE id = ?;

-- name: ListActiveAutomationRuns :many
SELECT id, automation_id, scheduled_for, session_id, status, attempt_count,
    claimed_at, lease_expires_at, started_at, finished_at, error_message,
    created_at, updated_at
FROM automation_runs
WHERE status IN ('spawning', 'running')
ORDER BY scheduled_for, id;

-- name: GetAutomationRunByOccurrence :one
SELECT id, automation_id, scheduled_for, session_id, status, attempt_count,
    claimed_at, lease_expires_at, started_at, finished_at, error_message,
    created_at, updated_at
FROM automation_runs
WHERE automation_id = ? AND scheduled_for = ?;

-- name: ListAutomationRuns :many
SELECT id, automation_id, scheduled_for, session_id, status, attempt_count,
    claimed_at, lease_expires_at, started_at, finished_at, error_message,
    created_at, updated_at
FROM automation_runs
WHERE automation_id = sqlc.arg(automation_id)
  AND (sqlc.arg(status_filter) = '' OR status = sqlc.arg(status_filter))
ORDER BY scheduled_for DESC, id DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountAutomationRuns :one
SELECT COUNT(*)
FROM automation_runs
WHERE automation_id = sqlc.arg(automation_id)
  AND (sqlc.arg(status_filter) = '' OR status = sqlc.arg(status_filter));

-- name: ListLatestAutomationRuns :many
SELECT r.id, r.automation_id, r.scheduled_for, r.session_id, r.status,
    r.attempt_count, r.claimed_at, r.lease_expires_at, r.started_at,
    r.finished_at, r.error_message, r.created_at, r.updated_at
FROM automation_runs r
WHERE r.automation_id IN (sqlc.slice('automation_ids'))
  AND r.id = (
      SELECT newest.id
      FROM automation_runs newest
      WHERE newest.automation_id = r.automation_id
      ORDER BY newest.scheduled_for DESC, newest.id DESC
      LIMIT 1
  )
ORDER BY r.automation_id;

-- name: GetNextClaimableAutomationRun :one
SELECT r.id, r.automation_id, r.scheduled_for, r.session_id, r.status,
    r.attempt_count, r.claimed_at, r.lease_expires_at, r.started_at,
    r.finished_at, r.error_message, r.created_at, r.updated_at
FROM automation_runs r
JOIN automations a ON a.id = r.automation_id
WHERE r.status = 'pending'
  AND a.enabled = 1
  AND NOT EXISTS (
      SELECT 1 FROM automation_runs active
      WHERE active.automation_id = r.automation_id
        AND active.status IN ('spawning', 'running')
  )
ORDER BY r.scheduled_for, r.id
LIMIT 1;

-- name: ClaimAutomationRun :execrows
UPDATE automation_runs
SET status = 'spawning', attempt_count = attempt_count + 1,
    claimed_at = ?, lease_expires_at = ?, error_message = NULL, updated_at = ?
WHERE automation_runs.id = ? AND automation_runs.status = 'pending'
  AND NOT EXISTS (
      SELECT 1 FROM automation_runs active
      WHERE active.automation_id = automation_runs.automation_id
        AND active.id <> automation_runs.id
        AND active.status IN ('spawning', 'running')
  );

-- name: MarkAutomationRunRunning :execrows
UPDATE automation_runs
SET status = 'running', session_id = ?, started_at = ?,
    lease_expires_at = NULL, error_message = NULL, updated_at = ?
WHERE id = ? AND status = 'spawning';

-- name: AutomationRunCanComplete :one
-- Read inside the same writer transaction as CompleteAutomationRun.
-- A completed visible user turn can finish a Chat run without
-- terminating its report session. Empty/queued/failed/interrupted chats and
-- pending input are deliberately not successful completion evidence.
-- Match Chat's active lineage and hidden-message rules, including inherited
-- history after a provider boundary and retired queued prompts after steering.
WITH RECURSIVE active_path(branch_id, max_sequence) AS (
    SELECT c.active_branch_id, CAST(NULL AS INTEGER)
    FROM conversations c
    JOIN automation_runs r ON r.session_id = c.current_session_id
    WHERE r.id = sqlc.arg(id)
    UNION ALL
    SELECT branch.parent_branch_id,
           CASE
               WHEN path.max_sequence IS NULL THEN branch.fork_after_sequence
               WHEN branch.fork_after_sequence < path.max_sequence THEN branch.fork_after_sequence
               ELSE path.max_sequence
           END
    FROM active_path path
    JOIN conversation_branches branch ON branch.id = path.branch_id
    WHERE branch.parent_branch_id IS NOT NULL
), latest_user AS (
    SELECT turn.*
    FROM conversation_messages message
    JOIN active_path path ON path.branch_id = message.branch_id
    LEFT JOIN conversation_turns turn ON turn.id = message.turn_id
    WHERE message.role = 'user'
      AND (path.max_sequence IS NULL OR message.sequence <= path.max_sequence)
      AND (message.turn_id IS NULL OR turn.id IS NULL OR (
          turn.rolled_back_at IS NULL AND turn.promoted_to_turn_id IS NULL AND turn.state <> 'cancelled'
      ))
    ORDER BY message.sequence DESC LIMIT 1
)
SELECT EXISTS (
SELECT 1 FROM automation_runs
WHERE automation_runs.id = sqlc.arg(id) AND automation_runs.status = 'running'
  AND EXISTS (
      SELECT 1 FROM sessions s
      WHERE s.id = automation_runs.session_id
        AND s.automation_run_id = automation_runs.id
        AND (
            s.is_terminated = 1
            OR (
                s.session_mode = 'chat' AND s.activity_state IN ('idle', 'exited')
                AND s.automation_launch_completed = 1
                AND EXISTS (
                    SELECT 1 FROM conversations c
                    JOIN latest_user t ON t.conversation_id = c.id
                    WHERE c.current_session_id = s.id
                      AND t.handled_by_session_id = s.id
                      AND t.state = 'completed' AND t.error_message = ''
                      AND t.completed_at IS NOT NULL
                      AND t.rolled_back_at IS NULL AND t.promoted_to_turn_id IS NULL
                      AND NOT EXISTS (
                          SELECT 1 FROM conversation_turns outstanding
                          JOIN active_path path ON path.branch_id = outstanding.branch_id
                          WHERE outstanding.conversation_id = c.id
                            AND outstanding.state IN ('queued', 'running')
                            AND outstanding.rolled_back_at IS NULL
                            AND outstanding.promoted_to_turn_id IS NULL
                            AND (path.max_sequence IS NULL OR EXISTS (
                                SELECT 1 FROM conversation_messages m
                                WHERE m.turn_id = outstanding.id AND m.sequence <= path.max_sequence
                            ))
                      )
                      AND NOT EXISTS (
                          SELECT 1 FROM conversation_activities interaction
                          WHERE interaction.conversation_id = c.id
                            AND interaction.kind IN ('approval', 'user_input')
                            AND interaction.status = 'pending'
                      )
                )
            )
        )
  )
);

-- name: CompleteAutomationRun :execrows
UPDATE automation_runs
SET status = 'completed', finished_at = ?, claimed_at = NULL,
    lease_expires_at = NULL, error_message = NULL, updated_at = ?
WHERE id = ? AND status = 'running';

-- name: FailAutomationRun :execrows
UPDATE automation_runs
SET status = 'failed', finished_at = ?, claimed_at = NULL,
    lease_expires_at = NULL, error_message = ?, updated_at = ?
WHERE id = ? AND status IN ('pending', 'spawning', 'running');

-- name: ReleaseAutomationRun :execrows
UPDATE automation_runs
SET status = 'pending', claimed_at = NULL, lease_expires_at = NULL,
    error_message = ?, updated_at = ?
WHERE id = ? AND status = 'spawning';
