-- +goose Up

-- The embedding of an L1 document's `text`, which is what similarity search
-- ranks on (docs/design.md#l1-distilled-documents). It is a column on the
-- document table rather than a store of its own: ADR-0004 puts every layer in
-- one Postgres, and a vector beside the row it belongs to needs no second write
-- to keep in step.
--
-- The width is l1.EmbeddingDimensions, and the two are one number: the process
-- that writes vectors compares the configured `embed` tier against it at
-- startup and refuses a tier that produces a different width
-- (llm.CheckDimensions), because a model of another width is this migration
-- plus a re-embed of every row, not a configuration edit.
--
-- NULL means "not embedded yet": a row written before an `embed` tier was
-- configured, or one whose text has just changed. l1.Store.Put sets it back to
-- NULL whenever it writes a different `text`, so a stale vector cannot outlive
-- the words it was made from, and search's full-text half still finds a
-- document that has no vector.
ALTER TABLE l1_docs ADD COLUMN embedding vector(1536);

-- There is deliberately no ANN index on this column. An HNSW or IVFFlat scan
-- ranks first and filters afterwards, and every read here is filtered by the
-- caller's ACL before ranking (docs/design.md#access-control), so an
-- approximate index would either drop documents the caller may read or rank
-- ones they may not. Search does an exact distance over the rows the filter
-- leaves, which is correct at any size and cheap at this one; the index, and
-- the filtering strategy that has to come with it, belong to whichever release
-- the table outgrows that.

-- +goose Down

ALTER TABLE l1_docs DROP COLUMN embedding;
