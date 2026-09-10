-- +goose Up

-- Every read of this table is filtered by the caller's ACL before anything is
-- ranked (docs/design.md#access-control), so the access list is a lookup key
-- and not just a column: search asks whether the list contains any of the
-- entries the caller satisfies, as a containment over jsonb.
--
-- jsonb_path_ops is the smaller index and supports @>, which is the only
-- operator that filter uses — the same choice, for the same reason, as the
-- index on `refs`. Containment ignores an entry's extra keys, which is what
-- lets a caller's `{kind, source, native_id}` match a stored entry that also
-- carries the source's human-readable `label`: two grants are the same grant
-- when those three agree, and the label is decoration (internal/l1).
CREATE INDEX l1_docs_acl_idx ON l1_docs USING gin (acl jsonb_path_ops);

-- +goose Down

DROP INDEX l1_docs_acl_idx;
