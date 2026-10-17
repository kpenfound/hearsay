-- +goose Up
-- Fresh journals need no backfill. Markers are identities only and append-only:
-- count visible rows, never sequence values or a counter updated by all writers.
DROP TABLE judgment_input_changes;
CREATE TABLE judgment_input_changes (
    kind text NOT NULL CHECK (kind IN ('source','artifact','thread','window','ledger','all')),
    source text NOT NULL DEFAULT '',
    key text NOT NULL DEFAULT '',
    bucket bigint NOT NULL DEFAULT 0
);
CREATE INDEX judgment_input_changes_scope ON judgment_input_changes(kind,source,key,bucket);

-- Retain the statement guard on every effective-input table, including no-op
-- writes. Row triggers below write membership markers in the writer's own
-- transaction. TRUNCATE has no rows to mark, so conservatively marks everything.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION judgment_guard_inputs() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    LOCK TABLE judgment_input_guard IN ROW EXCLUSIVE MODE;
    IF TG_OP = 'TRUNCATE' THEN
        INSERT INTO judgment_input_changes(kind) VALUES ('all');
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd

-- Record both the old and new memberships, and historical memberships when a
-- new revision or a tombstone arrives without its target's thread/container.
-- This covers a member moving out of a conversation as well as entering one.
-- +goose StatementBegin
CREATE FUNCTION judgment_mark_event(e l0_events) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO judgment_input_changes(kind,source,key) VALUES
        ('source',e.source,''), ('artifact',e.source,e.artifact);
    IF e.target IS NOT NULL THEN
        INSERT INTO judgment_input_changes(kind,source,key) VALUES ('artifact',e.source,e.target);
    END IF;
    INSERT INTO judgment_input_changes(kind,source,key,bucket)
    SELECT DISTINCT 'thread', v.source, coalesce(v.payload->>'thread',v.payload->>'parent'), 0
      FROM (SELECT e.* UNION ALL
            SELECT h.* FROM l0_events h WHERE h.source=e.source AND (h.artifact=e.artifact OR h.artifact=e.target)) v
     WHERE coalesce(v.payload->>'thread',v.payload->>'parent','') <> '';
    -- ChatWindow is 30 minutes, UTC. Use the same half-open buckets as L1.
    -- Revisions that change kind/thread still invalidate the old window,
    -- and tombstones can recover it from historical rows.
    INSERT INTO judgment_input_changes(kind,source,key,bucket)
    SELECT DISTINCT 'window', v.source, v.payload->'container'->>'native_id',
           floor(extract(epoch FROM v.occurred_at)/1800)::bigint
      FROM (SELECT e.* UNION ALL
            SELECT h.* FROM l0_events h WHERE h.source=e.source AND (h.artifact=e.artifact OR h.artifact=e.target)) v
     WHERE (v.kind='message' OR v.payload->>'base_kind'='message')
       AND coalesce(v.payload->>'thread','')=''
       AND v.payload->'container'->>'kind'='channel'
       AND coalesce(v.payload->'container'->>'native_id','') <> '';
END $$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION judgment_mark_inputs() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'l0_events' THEN
        IF TG_OP <> 'INSERT' THEN PERFORM judgment_mark_event(OLD); END IF;
        IF TG_OP <> 'DELETE' THEN PERFORM judgment_mark_event(NEW); END IF;
    ELSE
        IF TG_OP <> 'INSERT' THEN
            INSERT INTO judgment_input_changes(kind,key) SELECT 'ledger',unnest(OLD.topics);
        END IF;
        IF TG_OP <> 'DELETE' THEN
            INSERT INTO judgment_input_changes(kind,key) SELECT 'ledger',unnest(NEW.topics);
        END IF;
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER judgment_input_markers AFTER INSERT OR UPDATE OR DELETE ON l0_events
    FOR EACH ROW EXECUTE FUNCTION judgment_mark_inputs();
CREATE TRIGGER judgment_input_markers AFTER INSERT OR UPDATE OR DELETE ON l2_topic_operations
    FOR EACH ROW EXECUTE FUNCTION judgment_mark_inputs();

-- +goose Down
-- Discarding scoped history would make existing journal identities unsafe.
-- Restore from a database backup instead of pretending to reverse this change.
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'scoped judgment input history cannot be reversed'; END $$;
-- +goose StatementEnd
