-- name: GetLatestAgentLimitPeriod :one
SELECT * FROM agent_limit_periods
WHERE provider = ? AND login = ? AND window_id = ?
ORDER BY started_at_ms DESC
LIMIT 1;

-- name: UpsertAgentLimitPeriod :exec
INSERT INTO agent_limit_periods (
    provider, login, window_id, started_at_ms, email, account_id, plan, label, duration_minutes,
    resets_at_ms, peak_used_percent, last_used_percent, readings, last_read_at_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (provider, login, window_id, started_at_ms) DO UPDATE SET
    email = excluded.email,
    account_id = excluded.account_id,
    plan = excluded.plan,
    label = excluded.label,
    duration_minutes = excluded.duration_minutes,
    resets_at_ms = excluded.resets_at_ms,
    peak_used_percent = excluded.peak_used_percent,
    last_used_percent = excluded.last_used_percent,
    readings = excluded.readings,
    last_read_at_ms = excluded.last_read_at_ms;

-- name: UpsertAgentLimitDay :exec
INSERT INTO agent_limit_days (
    provider, login, window_id, day, email, label, duration_minutes, max_used_percent, readings, last_read_at_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
ON CONFLICT (provider, login, window_id, day) DO UPDATE SET
    email = excluded.email,
    label = excluded.label,
    duration_minutes = excluded.duration_minutes,
    max_used_percent = max(agent_limit_days.max_used_percent, excluded.max_used_percent),
    readings = agent_limit_days.readings + 1,
    last_read_at_ms = excluded.last_read_at_ms
WHERE excluded.last_read_at_ms > agent_limit_days.last_read_at_ms;

-- name: CountAgentLimitDays :one
SELECT count(*) FROM agent_limit_days;

-- name: DeleteAgentLimitPeriodsBefore :exec
DELETE FROM agent_limit_periods WHERE last_read_at_ms < ?;

-- name: DeleteAgentLimitDaysBefore :exec
DELETE FROM agent_limit_days WHERE day < ?;

-- name: ListAgentLimitPeriodsSince :many
SELECT * FROM agent_limit_periods
WHERE last_read_at_ms >= ?
ORDER BY provider, login, window_id, started_at_ms;

-- name: ListAgentLimitDaysSince :many
SELECT * FROM agent_limit_days
WHERE day >= ?
ORDER BY provider, login, window_id, day;
