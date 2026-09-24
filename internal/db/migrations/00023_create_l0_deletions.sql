-- +goose Up

-- Operator deletions (ADR-0018): the one case in which an L0 row is updated.
-- A deletion redacts the payload of the events it covers in place and keeps
-- the row — id, source, artifact, kind, time and ACL — so provenance and the
-- change feed stay intact. This table is the durable record of each one.
CREATE TABLE l0_deletions (
    -- `del_` and 32 hex digits, minted by `hearsay delete --apply`.
    id text PRIMARY KEY,

    -- The configured human principal who applied it. Never a free-text name.
    operator text NOT NULL,
    reason text NOT NULL CHECK (btrim(reason) <> ''),

    -- The selector as the operator gave it: event, artifact or author.
    selector jsonb NOT NULL,
    deleted_at timestamptz NOT NULL DEFAULT now(),

    -- The L0 events redacted, and the L1 documents the forward walk found and
    -- queued for re-distillation.
    events text[] NOT NULL CHECK (cardinality(events) > 0),
    documents text[] NOT NULL,

    -- The `deletion` event Hearsay wrote under source `hearsay`, which is how
    -- the deletion is on the change feed without looking like a tombstone.
    retraction text NOT NULL,

    -- How many times a connector re-emitted one of the covered events and the
    -- replay was dropped rather than restoring it.
    replays bigint NOT NULL DEFAULT 0
);

-- The deletion that redacted this row, or null. Every read hides a row that
-- has one, the way it hides a row a source tombstone covers.
ALTER TABLE l0_events ADD COLUMN deletion text REFERENCES l0_deletions (id);

-- +goose Down

-- Destroys the deletion records. The redacted payloads stay redacted: there is
-- nothing to restore them from, which is the point.
ALTER TABLE l0_events DROP COLUMN deletion;
DROP TABLE l0_deletions;
