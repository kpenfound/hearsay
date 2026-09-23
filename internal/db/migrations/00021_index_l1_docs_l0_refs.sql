-- +goose Up

-- `watch` asks, for every event it reads off the change feed, which documents
-- were built from it: `l0_refs @> ARRAY[event id]`. It is also what `get_l0`'s
-- reach check asks with `&&`. Without an index each question is a scan of the
-- whole table, and a watch asks it for every event in the feed.
CREATE INDEX l1_docs_l0_refs_idx ON l1_docs USING gin (l0_refs);

-- +goose Down

DROP INDEX l1_docs_l0_refs_idx;
