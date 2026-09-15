-- +goose Up

-- The connector runtime's ACL re-syncs: which containers of a source owe one,
-- how far the walk has got, and when the last one finished
-- (docs/connector-contract.md, "When only the access list changes";
-- ADR-0013).
--
-- A re-sync is what keeps L0 from serving a container that stopped being
-- public as public, so what is owed has to outlive the process that learned
-- it: a delivery answered and then a restart, or a walk half done and then a
-- deploy, would otherwise leave `public` on every artifact not yet re-emitted
-- with nothing left that knows.
--
-- This is not l0_backfill_cursors. That one is one walk per source, through
-- its history, and once done is never walked again. This one is per
-- container, and a container can owe a re-sync any number of times.
CREATE TABLE l0_resyncs (
    source text NOT NULL,

    -- The container's native id, as the ingest allowlist names it.
    container text NOT NULL,

    -- A re-sync is owed or under way. A row that is not owed is kept for
    -- resynced_at.
    owed boolean NOT NULL DEFAULT true,

    -- Where the owed re-sync has got to, opaque as a backfill cursor is. The
    -- empty string is its beginning.
    cursor text NOT NULL DEFAULT '',

    -- Counts the requests. A request starts the walk again, because part of
    -- a walk under way may have read the container before it changed; a walk
    -- saves its cursor and clears owed only while the generation is the one it
    -- read, so a walk that began before a request cannot settle it.
    generation bigint NOT NULL DEFAULT 1,

    -- When the last re-sync of the container finished. A public artifact that
    -- arrived before it is one the walk could not reach, and the startup check
    -- does not re-sync the container again for it.
    resynced_at timestamptz,

    updated_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (source, container),

    CONSTRAINT l0_resyncs_source_is_a_source_id
        CHECK (source ~ '^[a-z0-9][a-z0-9_-]*$' AND octet_length(source) <= 64),
    CONSTRAINT l0_resyncs_container_is_named
        CHECK (container <> ''),
    CONSTRAINT l0_resyncs_cursor_fits
        CHECK (octet_length(cursor) <= 4096),
    CONSTRAINT l0_resyncs_generation_is_positive
        CHECK (generation > 0)
);

-- +goose Down

-- Forgets every owed re-sync. The next start's check still finds a container
-- L0 serves as public that the source no longer does, and owes it again.
DROP TABLE l0_resyncs;
