-- +goose Up
ALTER TABLE pipeline_observations ADD COLUMN generative_attempts bigint CHECK (generative_attempts >= 0);

-- +goose Down
ALTER TABLE pipeline_observations DROP COLUMN generative_attempts;
