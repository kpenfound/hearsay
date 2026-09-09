-- +goose Up

-- The L0 event table: one row per event, in the shape a connector emits
-- (docs/design.md#l0-events, docs/connector-contract.md). It is append-only.
-- Nothing updates or deletes a row; a deletion at the source arrives as a
-- tombstone event naming the artifact it retracts, and reads exclude what a
-- tombstone covers while the rows stay (docs/design.md#deletion-and-provenance).
CREATE TABLE l0_events (
    -- The event id, `evt:<source>:<native id>`, derived from source and
    -- native_id by connector.EventID. Making it the primary key is the whole
    -- of ingest idempotency: re-emitting an artifact that has not changed
    -- produces the same id and conflicts, and an edit carries a new revision
    -- token in its native id and so is a new row.
    id text PRIMARY KEY,

    -- seq and xact_id together are the change feed's cursor. A serial alone
    -- would not do: sequence values are handed out before commit, so a
    -- transaction holding a low seq can commit after one holding a high seq,
    -- and a reader that had already passed the high one would never see the
    -- low one. Ordering by (xact_id, seq) and serving only rows whose xact_id
    -- is below pg_snapshot_xmin(pg_current_snapshot()) — transactions that
    -- have finished — makes the feed's order the order in which rows become
    -- readable, so a cursor can never skip an event. The cost is that the feed
    -- waits behind a long-running write transaction rather than reading past
    -- it.
    seq bigint GENERATED ALWAYS AS IDENTITY,
    xact_id xid8 NOT NULL DEFAULT pg_current_xact_id(),

    -- The event's own fields. source is the configured source instance
    -- (`github-acme`), native_id one observation of an artifact within it.
    source text NOT NULL,
    native_id text NOT NULL,
    kind text NOT NULL,

    -- Lifted out of payload because reads index on them: artifact is what
    -- makes an artifact's history queryable, revision_* is what orders that
    -- history, and target is what a tombstone retracts. They are copies of
    -- payload.artifact, payload.revision.* and payload.target, held to those
    -- values by the ingest path, which stores an event only after
    -- connector.Event.Validate has passed.
    artifact text NOT NULL,
    revision_token text,
    revision_edited_at timestamptz,
    target text,

    -- occurred_at is the contract's `time`: when the artifact happened at the
    -- source, identical on every revision of it. ingested_at is when Hearsay
    -- saw this row. Both are microsecond resolution, which is Postgres's.
    occurred_at timestamptz NOT NULL,
    ingested_at timestamptz NOT NULL DEFAULT now(),

    -- The payload and the access list the source described at ingest
    -- (docs/design.md#access-control). jsonb rather than json so that two
    -- spellings of one payload compare equal, which is what lets ingest tell a
    -- replay from a rewrite.
    payload jsonb NOT NULL,
    acl jsonb NOT NULL,

    -- The contract's rules about the native id, stated where nothing can get
    -- past them. connector.Event.Validate enforces the same ones in Go; these
    -- hold for a row written by anything else.
    CONSTRAINT l0_events_native_id_is_the_artifact_or_a_revision CHECK (
        CASE WHEN revision_token IS NULL
             THEN native_id = artifact
             ELSE native_id = artifact || '@' || revision_token
        END
    ),
    -- Revisions of one artifact share occurred_at, so edited_at is what orders
    -- them; one that precedes the artifact would sort an edit before the thing
    -- it edits.
    CONSTRAINT l0_events_revision_edited_at_is_the_revisions_own CHECK (
        revision_edited_at IS NULL
        OR (revision_token IS NOT NULL AND revision_edited_at >= occurred_at)
    ),
    -- A tombstone is its own artifact (`<target>:tombstone`), so that it is
    -- emitted the same way twice and does not read as a revision of what it
    -- retracts. A row whose target is its own artifact would retract itself.
    CONSTRAINT l0_events_tombstone_is_its_own_artifact CHECK (
        target IS NULL OR target <> artifact
    ),
    -- An event nobody may read cannot be read back, so an empty access list is
    -- a bug rather than a private event.
    CONSTRAINT l0_events_acl_is_not_empty CHECK (
        jsonb_typeof(acl) = 'array' AND jsonb_array_length(acl) > 0
    )
);

-- The change feed's index: the order Changes reads in.
CREATE INDEX l0_events_feed_idx ON l0_events (xact_id, seq);

-- Every read excludes events a tombstone covers, which is an anti-join on
-- (source, target). Partial, because only tombstones carry a target.
CREATE INDEX l0_events_target_idx ON l0_events (source, target) WHERE target IS NOT NULL;

-- An artifact's history, in the order the contract puts its revisions in: the
-- observation with no edit time first, then by edit time, then by arrival.
CREATE INDEX l0_events_artifact_idx
    ON l0_events (source, artifact, revision_edited_at NULLS FIRST, seq);

-- Listing and counting by source, and by source and kind.
CREATE INDEX l0_events_source_kind_idx ON l0_events (source, kind, occurred_at DESC);

-- +goose Down

-- Honest, and it destroys every event. `hearsay migrate down` is a development
-- convenience that refuses without --i-know (ADR-0006); recovery in production
-- is a restore, not a down migration.
DROP TABLE l0_events;
