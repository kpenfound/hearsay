-- +goose Up
-- Preserve each member identity alongside its conversation marker. A concurrent
-- tombstone/revision may not see the member's uncommitted history in its trigger.
-- Capture follows artifact markers through these immutable membership identities
-- in its own snapshot, including members omitted from the question by ACL folding.
-- No backfill: fresh journals start with the complete marker protocol.
ALTER TABLE judgment_input_changes ADD COLUMN member_artifact text NOT NULL DEFAULT '';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION judgment_mark_event(e l0_events) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO judgment_input_changes(kind,source,key) VALUES
        ('source',e.source,''), ('artifact',e.source,e.artifact);
    IF e.target IS NOT NULL THEN
        INSERT INTO judgment_input_changes(kind,source,key) VALUES ('artifact',e.source,e.target);
    END IF;
    INSERT INTO judgment_input_changes(kind,source,key,bucket,member_artifact)
    SELECT DISTINCT 'thread', v.source, coalesce(v.payload->>'thread',v.payload->>'parent'), 0, v.artifact
      FROM (SELECT e.* UNION ALL
            SELECT h.* FROM l0_events h WHERE h.source=e.source AND (h.artifact=e.artifact OR h.artifact=e.target)) v
     WHERE coalesce(v.payload->>'thread',v.payload->>'parent','') <> '';
    -- ChatWindow is 30 minutes, UTC. Use the same half-open buckets as L1.
    -- Revisions that change kind/thread still invalidate the old window,
    -- and tombstones can recover it from historical rows.
    INSERT INTO judgment_input_changes(kind,source,key,bucket,member_artifact)
    SELECT DISTINCT 'window', v.source, v.payload->'container'->>'native_id',
           floor(extract(epoch FROM v.occurred_at)/1800)::bigint, v.artifact
      FROM (SELECT e.* UNION ALL
            SELECT h.* FROM l0_events h WHERE h.source=e.source AND (h.artifact=e.artifact OR h.artifact=e.target)) v
     WHERE (v.kind='message' OR v.payload->>'base_kind'='message')
       AND coalesce(v.payload->>'thread','')=''
       AND v.payload->'container'->>'kind'='channel'
       AND coalesce(v.payload->'container'->>'native_id','') <> '';
END $$;
-- +goose StatementEnd

-- +goose Down
-- Removing member identities would weaken validation of stored input snapshots.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'scoped judgment input history cannot be reversed'; END $$;
-- +goose StatementEnd
