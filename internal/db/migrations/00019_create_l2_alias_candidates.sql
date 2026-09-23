-- +goose Up

-- Proposed names do not participate in resolve. A vote belongs to one L1
-- document; the PR is evidence that connects the name to changed code.
CREATE TABLE l2_alias_candidates (
    entity_id text NOT NULL,
    alias text NOT NULL,
    name text NOT NULL,
    votes integer NOT NULL DEFAULT 0,
    evidence text[] NOT NULL DEFAULT '{}',
    acl jsonb NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (entity_id, alias),
    CONSTRAINT l2_alias_votes_nonnegative CHECK (votes >= 0)
);
CREATE TABLE l2_alias_votes (
    entity_id text NOT NULL,
    alias text NOT NULL,
    doc_id text NOT NULL,
    pr_doc_id text NOT NULL,
    PRIMARY KEY (entity_id, alias, doc_id),
    FOREIGN KEY (entity_id, alias) REFERENCES l2_alias_candidates(entity_id, alias) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE l2_alias_votes;
DROP TABLE l2_alias_candidates;
