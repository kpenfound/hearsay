-- +goose Up

-- Fixed channel windows are read by source, container and source time.
CREATE INDEX l0_events_chat_window_idx ON l0_events
    (source, (payload->'container'->>'native_id'), occurred_at)
    WHERE coalesce(payload->>'base_kind', kind) = 'message';

-- +goose Down

DROP INDEX l0_events_chat_window_idx;
