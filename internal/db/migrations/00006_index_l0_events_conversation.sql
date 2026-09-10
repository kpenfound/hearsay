-- +goose Up

-- The index behind reading one conversation: every event that hangs off an
-- artifact, which is what an L1 document is assembled from (a pull request with
-- its reviews and its comments is one document).
--
-- The expression is docs/connector-contract.md's own rule — `thread` is the
-- root of the conversation, and a source with no threads gives `parent`, which
-- on a two-level source is the same value. Indexing the coalesce rather than
-- the two columns is what keeps l0.Filter.Thread one predicate that either
-- source answers.
--
-- Not partial. The obvious `WHERE payload ? 'thread' OR payload ? 'parent'`
-- would halve it — most events hang off nothing — but the planner has to prove
-- a query's predicate implies the index's before it may use one, and it does
-- not prove that from an equality on the coalesce. An index Postgres declines
-- to use is worse than a bigger one it uses.
CREATE INDEX l0_events_conversation_idx
    ON l0_events (source, (coalesce(payload->>'thread', payload->>'parent')));

-- +goose Down

DROP INDEX l0_events_conversation_idx;
