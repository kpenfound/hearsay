-- +goose Up

-- Deferred container access starts from every artifact observed in a container,
-- then uses l0_events_artifact_idx to read each artifact's current revision.
-- The message-only chat-window index cannot serve repositories' other kinds.
CREATE INDEX l0_events_container_idx ON l0_events
    (source, (payload->'container'->>'native_id'), artifact);

-- +goose Down

DROP INDEX l0_events_container_idx;
