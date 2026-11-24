-- +goose Up

-- The full-text half of search reads a document's distilled summary and outcome
-- as well as `raw_text`, so a distillation can be found by the words it was
-- written in with no embed tier configured (docs/design.md#l1-distilled-documents).
--
-- The text search configuration is named, as in migration 8: an index built
-- under one configuration and queried under another silently matches nothing,
-- and every query in internal/l1 names `english` the same way. The expression
-- is the one `searchDocument` holds in internal/l1/search.go; the query has
-- to spell it exactly as it is spelled here for the planner to use the index.
-- `summary` and `outcome` are the JSON names of l1.Body's fields, and either may
-- be absent from a document's body.
--
-- Migration 8's index over `raw_text` alone is replaced: this one covers
-- `raw_text` too, and keeping both would pay to maintain an index nothing reads.
CREATE INDEX l1_docs_search_fts_idx ON l1_docs USING gin ((
    to_tsvector('english', raw_text)
    || to_tsvector('english', coalesce(body ->> 'summary', ''))
    || to_tsvector('english', coalesce(body ->> 'outcome', ''))
));

DROP INDEX l1_docs_raw_text_fts_idx;

-- +goose Down

CREATE INDEX l1_docs_raw_text_fts_idx ON l1_docs USING gin (to_tsvector('english', raw_text));

DROP INDEX l1_docs_search_fts_idx;
