-- +goose Up
-- Version 14 from the stance branch has no artifact_class. On the merged
-- history, version 14 already supplied it, so this is a no-op for that path.
ALTER TABLE l1_docs ADD COLUMN IF NOT EXISTS artifact_class text;

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
WHERE root.id = d.l0_refs[1] AND d.artifact_class IS NULL;

ALTER TABLE l1_docs ALTER COLUMN artifact_class SET NOT NULL;
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'l1_docs'::regclass
                   AND conname = 'l1_docs_artifact_class_is_known') THEN
        ALTER TABLE l1_docs ADD CONSTRAINT l1_docs_artifact_class_is_known CHECK (
            artifact_class IN ('merged_pr', 'spec', 'meeting', 'issue', 'pull_request',
                               'commit', 'chat_thread', 'dm', 'agent')
        );
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- The column belongs to migration 14 in the canonical history. Keep it for
-- that migration's down; dropping it here would destroy existing class data.
SELECT 1;
