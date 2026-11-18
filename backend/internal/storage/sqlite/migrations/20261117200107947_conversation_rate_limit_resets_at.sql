-- +goose Up
-- Absolute reset instants (RFC 3339, UTC). The old resets_in columns held
-- seconds remaining at write time, which froze once the row aged.
ALTER TABLE conversations ADD COLUMN rate_limit_primary_resets_at TEXT;
ALTER TABLE conversations ADD COLUMN rate_limit_secondary_resets_at TEXT;

-- +goose Down
ALTER TABLE conversations DROP COLUMN rate_limit_secondary_resets_at;
ALTER TABLE conversations DROP COLUMN rate_limit_primary_resets_at;
