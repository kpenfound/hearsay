-- +goose Up

-- Reads follow the topic ledger (issue #172, ADR-0021): a stance can be
-- superseded by one that is on another topic now, so whether a later reading
-- retired it, and which stances supersede it, are read by the supersedes edge
-- rather than through the topic's index.
CREATE INDEX l2_stances_supersedes_idx ON l2_stances (supersedes) WHERE supersedes IS NOT NULL;

-- +goose Down

DROP INDEX l2_stances_supersedes_idx;
