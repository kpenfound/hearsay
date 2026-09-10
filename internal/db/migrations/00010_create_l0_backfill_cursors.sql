-- +goose Up

-- Where each source's backfill has got to: the connector runtime's position in
-- a walk through one source's history.
--
-- It is kept here rather than in the process for the reason every other cursor
-- is: a connector is restarted, scaled and replaced, and a position held in
-- memory would start every source's history again on every deploy. History is
-- the expensive half of ingest — it is the whole of a source rather than what
-- changed since the last poll.
--
-- This is not l0_feed_cursors. That one is a *reader's* position in the change
-- feed, in the two parts an l0.Cursor is, and it only ever moves forward. This
-- one is a *connector's* position in a source's history, and it is opaque:
-- only the connector that produced a cursor can compare two of them, so there
-- is no ordering to enforce here.
CREATE TABLE l0_backfill_cursors (
    -- The source id, one row per source rather than per replica or per
    -- process: two processes hosting a source share its walk through history.
    source text PRIMARY KEY,

    -- What the connector handed back, stored and given to it again on the next
    -- call. Opaque to Hearsay: a page token, a timestamp, a JSON object. The
    -- empty string is the beginning of history, which is what the first call
    -- gets.
    cursor text NOT NULL,

    -- History is exhausted and the runtime has stopped calling Backfill. This
    -- is what makes a restart resume a finished backfill rather than walk the
    -- source again from the beginning.
    done boolean NOT NULL DEFAULT false,

    -- How many events the backfill has emitted in total, for progress. It is
    -- the connector's own count summed over its calls, so it says what the
    -- connector claims to have emitted rather than what reached l0_events —
    -- re-emitting an event Hearsay already has writes nothing.
    events bigint NOT NULL DEFAULT 0,

    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l0_backfill_cursors_source_is_a_source_id
        CHECK (source ~ '^[a-z0-9][a-z0-9_-]*$' AND octet_length(source) <= 64),
    CONSTRAINT l0_backfill_cursors_cursor_fits
        CHECK (octet_length(cursor) <= 4096),
    CONSTRAINT l0_backfill_cursors_events_is_not_negative
        CHECK (events >= 0)
);

-- +goose Down

-- Destroys every source's backfill position, so the next start walks each
-- source's history from the beginning again. Nothing is lost — ingest is
-- idempotent on the event id and a re-emitted event writes nothing — but the
-- walk is paid for a second time.
DROP TABLE l0_backfill_cursors;
