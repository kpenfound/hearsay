-- +goose Up
ALTER TABLE l2_entities DROP CONSTRAINT l2_entities_origin_is_known;
ALTER TABLE l2_entities ADD CONSTRAINT l2_entities_origin_is_known
    CHECK (origin IN ('config', 'repo_structure', 'reference', 'source'));

-- +goose Down
-- Refuse to narrow the origin set while source entities remain.
ALTER TABLE l2_entities DROP CONSTRAINT l2_entities_origin_is_known;
ALTER TABLE l2_entities ADD CONSTRAINT l2_entities_origin_is_known
    CHECK (origin IN ('config', 'repo_structure', 'reference'));
