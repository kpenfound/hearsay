-- +goose Up

-- Where each consumer of the L0 change feed has got to. The distiller is the
-- first one; `watch` will be another.
--
-- The position is kept here rather than in the consumer because a consumer is a
-- stateless process that is restarted, scaled and replaced: a cursor that lived
-- in one would re-read the feed from the beginning on every restart, and
-- re-reading the feed means re-enqueueing a distillation for every artifact
-- Hearsay has ever ingested.
CREATE TABLE l0_feed_cursors (
    -- Who is reading: `distiller`. One row per consumer, not per replica —
    -- replicas of one service share a position, and the write is a
    -- move-forward-only upsert so that two of them cannot pull it backwards.
    consumer text PRIMARY KEY,

    -- The cursor, in the two parts l0.Cursor is: the transaction that wrote the
    -- row and the row's position within it. Two columns rather than the text
    -- form, so that the comparison that keeps the cursor moving forward is the
    -- same one the feed itself orders by.
    xact_id xid8 NOT NULL,
    seq bigint NOT NULL,

    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l0_feed_cursors_seq_is_not_negative CHECK (seq >= 0)
);

-- +goose Down

-- Destroys every consumer's position, so the next start re-reads the feed from
-- the beginning: nothing is lost, and everything is distilled again.
DROP TABLE l0_feed_cursors;
