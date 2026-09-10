-- +goose Up

-- The full-text half of hybrid search (docs/design.md#l1-distilled-documents).
-- It is over `raw_text`, the team's own words, and never over `text`: the
-- distillation is what the embedding covers, and indexing both would rank one
-- document twice for the same sentence said once.
--
-- The text search configuration is named rather than left to
-- default_text_search_config, which is a session setting nothing here pins: an
-- index built under one configuration and queried under another silently
-- matches nothing. Every query in internal/l1 names `english` the same way. A
-- deployment whose team writes in another language is a configuration question
-- this build does not answer yet.
--
-- It is an expression index rather than a stored tsvector column: the column
-- would rewrite the table to add and would have to be kept in step, and the
-- expression is the same idiom migration 6 uses for the L0 conversation key.
CREATE INDEX l1_docs_raw_text_fts_idx ON l1_docs USING gin (to_tsvector('english', raw_text));

-- +goose Down

DROP INDEX l1_docs_raw_text_fts_idx;
