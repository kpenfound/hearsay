-- +goose Up
-- Expand, derive from retained L0 provenance, then close the constraint in one
-- transaction. Re-running an interrupted migration repeats the same derivation.
ALTER TABLE l1_docs ADD COLUMN artifact_class text;

UPDATE l1_docs AS d SET artifact_class = CASE
    WHEN d.kind = 'pr' AND (
        NULLIF(root.payload #>> '{native,merged_at}', '') IS NOT NULL
        OR root.payload #>> '{native,state}' = 'merged'
    ) THEN 'merged_pr'
    WHEN d.kind = 'pr' THEN 'pull_request'
    WHEN d.kind = 'meeting_segment' THEN 'meeting'
    WHEN d.kind = 'wiki_section' THEN 'spec'
    WHEN d.kind = 'issue' THEN 'issue'
    WHEN d.kind = 'commit' THEN 'commit'
    WHEN d.kind IN ('chat_thread', 'chat_burst')
        AND root.payload #>> '{container,kind}' IN ('dm', 'direct_message') THEN 'dm'
    WHEN d.kind IN ('chat_thread', 'chat_burst') THEN 'chat_thread'
    ELSE NULL
END
FROM l0_events AS root
WHERE root.id = d.l0_refs[1];

ALTER TABLE l1_docs ALTER COLUMN artifact_class SET NOT NULL;
ALTER TABLE l1_docs ADD CONSTRAINT l1_docs_artifact_class_is_known CHECK (
    artifact_class IN ('merged_pr', 'spec', 'meeting', 'issue', 'pull_request',
                       'commit', 'chat_thread', 'dm', 'agent')
);

-- +goose Down
ALTER TABLE l1_docs DROP COLUMN artifact_class;
