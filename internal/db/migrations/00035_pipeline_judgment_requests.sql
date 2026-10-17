-- +goose Up
-- Logical observations include cooldown suppressions; retries are attempts.
ALTER TABLE pipeline_observations ADD COLUMN judgment_requests bigint CHECK (judgment_requests >= 0);

-- +goose Down
ALTER TABLE pipeline_observations DROP COLUMN judgment_requests;
