-- +goose Up

-- What an operator deletion rebuilt (issue #23, Decisions 7 and 9). The
-- deletion record in l0_deletions says what the forward walk found; these say
-- what became of it.

-- One row per L1 document a deletion queued, written by the distiller in the
-- transaction that re-distilled or deleted it. A document with no row here has
-- not been rebuilt yet. The first rebuild after the deletion is the one
-- recorded: a later re-distillation of the same document is not the
-- deletion's.
CREATE TABLE l0_deletion_rebuilds (
    deletion text NOT NULL REFERENCES l0_deletions (id),
    document text NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('redistilled', 'deleted')),
    rebuilt_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (deletion, document)
);

CREATE INDEX l0_deletion_rebuilds_document_idx ON l0_deletion_rebuilds (document);
CREATE INDEX l0_deletions_documents_idx ON l0_deletions USING gin (documents);

-- The deletion that redacted this row's text, or null. A stance written before
-- a deletion rebuilt one of its documents has its position replaced once it is
-- superseded; a topic whose opening document the deletion removed, and which
-- no surviving document supports, has its name replaced. The row, its id and
-- its supersession edge stay.
ALTER TABLE l2_stances ADD COLUMN redacted_by text REFERENCES l0_deletions (id);
ALTER TABLE l2_topics ADD COLUMN redacted_by text REFERENCES l0_deletions (id);

-- +goose Down

-- The redacted text stays redacted: there is nothing to restore it from.
ALTER TABLE l2_topics DROP COLUMN redacted_by;
ALTER TABLE l2_stances DROP COLUMN redacted_by;
DROP INDEX l0_deletions_documents_idx;
DROP TABLE l0_deletion_rebuilds;
