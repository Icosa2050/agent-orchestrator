-- +goose Up
-- One row per login, limit window and reset period: how full the window got before it reset.
CREATE TABLE agent_limit_periods (
    provider          TEXT    NOT NULL CHECK (provider IN ('claude', 'codex')),
    login             TEXT    NOT NULL CHECK (trim(login) <> ''),
    window_id         TEXT    NOT NULL CHECK (trim(window_id) <> ''),
    started_at_ms     INTEGER NOT NULL,
    email             TEXT    NOT NULL DEFAULT '',
    account_id        TEXT    NOT NULL DEFAULT '',
    plan              TEXT    NOT NULL DEFAULT '',
    label             TEXT    NOT NULL DEFAULT '',
    duration_minutes  INTEGER NOT NULL DEFAULT 0,
    resets_at_ms      INTEGER NOT NULL,
    peak_used_percent REAL    NOT NULL,
    last_used_percent REAL    NOT NULL,
    readings          INTEGER NOT NULL,
    last_read_at_ms   INTEGER NOT NULL,
    PRIMARY KEY (provider, login, window_id, started_at_ms)
) WITHOUT ROWID;

CREATE INDEX agent_limit_periods_last_read_at ON agent_limit_periods (last_read_at_ms);

-- One row per login, limit window and UTC day: the fullest reading that day.
CREATE TABLE agent_limit_days (
    provider         TEXT    NOT NULL CHECK (provider IN ('claude', 'codex')),
    login            TEXT    NOT NULL CHECK (trim(login) <> ''),
    window_id        TEXT    NOT NULL CHECK (trim(window_id) <> ''),
    day              TEXT    NOT NULL,
    email            TEXT    NOT NULL DEFAULT '',
    label            TEXT    NOT NULL DEFAULT '',
    duration_minutes INTEGER NOT NULL DEFAULT 0,
    max_used_percent REAL    NOT NULL,
    readings         INTEGER NOT NULL,
    last_read_at_ms  INTEGER NOT NULL,
    PRIMARY KEY (provider, login, window_id, day)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE agent_limit_days;
DROP TABLE agent_limit_periods;
