-- +goose Up

-- A `work` source's decisions are L1 documents of their own class, so that an
-- authority policy can ratify decisions apart from specs. The class is a
-- constrained set, so it takes a migration, as every class has.
ALTER TABLE l1_docs DROP CONSTRAINT l1_docs_artifact_class_is_known;
ALTER TABLE l1_docs ADD CONSTRAINT l1_docs_artifact_class_is_known CHECK (
    artifact_class IN ('merged_pr', 'spec', 'decision', 'meeting', 'issue', 'pull_request',
                       'commit', 'chat_thread', 'dm', 'agent')
);

-- A decision's supersession is read from the decisions of its project, which
-- the structured body names: reads look up the decisions of one work source's
-- project, and the ones that name a decision id.
CREATE INDEX l1_docs_decisions_by_project ON l1_docs (
    (body->'decision'->>'work_source'), (body->'decision'->>'project'), (body->'decision'->>'supersedes')
) WHERE kind = 'decision';

-- +goose Down
DROP INDEX l1_docs_decisions_by_project;
ALTER TABLE l1_docs DROP CONSTRAINT l1_docs_artifact_class_is_known;
DELETE FROM l1_docs WHERE artifact_class = 'decision';
ALTER TABLE l1_docs ADD CONSTRAINT l1_docs_artifact_class_is_known CHECK (
    artifact_class IN ('merged_pr', 'spec', 'meeting', 'issue', 'pull_request',
                       'commit', 'chat_thread', 'dm', 'agent')
);
