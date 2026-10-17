-- +goose Up

-- All effective-input writers take ROW EXCLUSIVE on this table before writing.
-- A journal commit holds SHARE ROW EXCLUSIVE until the caller commits, closing the interval
-- between validation and the consumer's commit. L0 and ledger changes
-- conservatively invalidate all prior inputs. Journal commits serialize only
-- the short validation/write interval, never provider calls.
CREATE TABLE judgment_input_guard (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton)
);
INSERT INTO judgment_input_guard DEFAULT VALUES;

-- Count visible committed changes, not sequence values (which can be handed
-- out before a transaction commits). Append rather than updating one counter:
-- concurrent L0 writers must be able to commit in either order. This log holds
-- only a category, never evidence. Do not prune it independently of the journal.
CREATE TABLE judgment_input_changes (
    kind text NOT NULL CHECK (kind IN ('source', 'ledger'))
);
CREATE INDEX judgment_input_changes_kind ON judgment_input_changes(kind);

-- +goose StatementBegin
CREATE FUNCTION judgment_guard_inputs() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    LOCK TABLE judgment_input_guard IN ROW EXCLUSIVE MODE;
    IF TG_TABLE_NAME = 'l0_events' THEN
        INSERT INTO judgment_input_changes VALUES ('source');
    ELSIF TG_TABLE_NAME = 'l2_topic_operations' THEN
        INSERT INTO judgment_input_changes VALUES ('ledger');
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER judgment_inputs BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON l0_events
    FOR EACH STATEMENT EXECUTE FUNCTION judgment_guard_inputs();
CREATE TRIGGER judgment_inputs BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON l1_docs
    FOR EACH STATEMENT EXECUTE FUNCTION judgment_guard_inputs();
CREATE TRIGGER judgment_inputs BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON l2_topics
    FOR EACH STATEMENT EXECUTE FUNCTION judgment_guard_inputs();
CREATE TRIGGER judgment_inputs BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON l2_stances
    FOR EACH STATEMENT EXECUTE FUNCTION judgment_guard_inputs();
CREATE TRIGGER judgment_inputs BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON l2_topic_operations
    FOR EACH STATEMENT EXECUTE FUNCTION judgment_guard_inputs();
CREATE TRIGGER judgment_inputs BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON l0_deletions
    FOR EACH STATEMENT EXECUTE FUNCTION judgment_guard_inputs();

CREATE TABLE judgment_journal (
    id text PRIMARY KEY,
    task text NOT NULL,
    question_version text NOT NULL,
    question_hash text NOT NULL,
    inputs jsonb NOT NULL,
    input_hash text NOT NULL,
    documents text[] NOT NULL,
    events text[] NOT NULL,
    scope text[] NOT NULL,
    acl jsonb NOT NULL CHECK (jsonb_typeof(acl) = 'array' AND jsonb_array_length(acl) > 0),
    state text NOT NULL CHECK (state IN ('attempt', 'committed')),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    committed_at timestamptz,
    model_version text NOT NULL DEFAULT '',
    mode text NOT NULL CHECK (mode IN ('shadow', 'accepted')),
    policy text NOT NULL DEFAULT '',
    outcome text CHECK (outcome IN ('answer', 'abstain', 'fallback')),
    label text,
    fallback_reason text,
    probabilities jsonb,
    confidence double precision CHECK (confidence BETWEEN 0 AND 1),
    attempts bigint CHECK (attempts >= 0),
    input_tokens bigint CHECK (input_tokens >= 0),
    output_tokens bigint CHECK (output_tokens >= 0),
    cost_usd double precision CHECK (cost_usd >= 0 AND cost_usd < 'Infinity'::float8),
    CHECK (mode <> 'accepted' OR policy <> ''),
    CHECK ((state = 'attempt' AND committed_at IS NULL AND outcome IS NULL)
        OR (state = 'committed' AND committed_at IS NOT NULL AND outcome IS NOT NULL)),
    CHECK (outcome IS DISTINCT FROM 'fallback' OR fallback_reason <> '')
);
CREATE INDEX judgment_journal_lookup ON judgment_journal(task, question_version, question_hash, input_hash)
    WHERE state = 'committed';
CREATE INDEX judgment_journal_documents ON judgment_journal USING gin(documents);
CREATE INDEX judgment_journal_events ON judgment_journal USING gin(events);

CREATE TABLE pipeline_observations (
    id text PRIMARY KEY,
    stage text NOT NULL,
    cohort text NOT NULL CHECK (cohort IN ('baseline', 'shadow', 'accepted')),
    documents text[] NOT NULL,
    events text[] NOT NULL,
    scope text[] NOT NULL,
    acl jsonb NOT NULL CHECK (jsonb_typeof(acl) = 'array' AND jsonb_array_length(acl) > 0),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    handler_ns bigint CHECK (handler_ns >= 0),
    queue_delay_ns bigint CHECK (queue_delay_ns >= 0),
    generative_calls bigint CHECK (generative_calls >= 0),
    generative_input_tokens bigint CHECK (generative_input_tokens >= 0),
    generative_output_tokens bigint CHECK (generative_output_tokens >= 0),
    generative_cost_usd double precision CHECK (generative_cost_usd >= 0 AND generative_cost_usd < 'Infinity'::float8),
    judgment_attempts bigint CHECK (judgment_attempts >= 0),
    judgment_fallbacks bigint CHECK (judgment_fallbacks >= 0),
    judgment_input_tokens bigint CHECK (judgment_input_tokens >= 0),
    judgment_output_tokens bigint CHECK (judgment_output_tokens >= 0),
    judgment_cost_usd double precision CHECK (judgment_cost_usd >= 0 AND judgment_cost_usd < 'Infinity'::float8),
    candidate_count bigint CHECK (candidate_count >= 0)
);
CREATE INDEX pipeline_observations_documents ON pipeline_observations USING gin(documents);
CREATE INDEX pipeline_observations_events ON pipeline_observations USING gin(events);

-- Deletion removes even interrupted attempts and evidence from a compared
-- position or candidate. Kept at the database boundary so every operator
-- deletion path, including l0.Delete, has the same privacy behaviour.
-- +goose StatementBegin
CREATE FUNCTION judgment_delete_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM judgment_journal WHERE documents && NEW.documents OR events && NEW.events;
    DELETE FROM pipeline_observations WHERE documents && NEW.documents OR events && NEW.events;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER judgment_delete_evidence AFTER INSERT ON l0_deletions
    FOR EACH ROW EXECUTE FUNCTION judgment_delete_evidence();

-- No interrupted attempt or observation can race a deletion and recreate a
-- reference after its deletion trigger has already run.
-- +goose StatementBegin
CREATE FUNCTION judgment_check_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    LOCK TABLE judgment_input_guard IN SHARE MODE;
    IF EXISTS (SELECT 1 FROM l0_deletions d WHERE d.documents && NEW.documents OR d.events && NEW.events) THEN
        RAISE EXCEPTION 'judgment evidence was deleted';
    END IF;
    RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER judgment_check_evidence BEFORE INSERT OR UPDATE ON judgment_journal
    FOR EACH ROW EXECUTE FUNCTION judgment_check_evidence();
CREATE TRIGGER judgment_check_evidence BEFORE INSERT ON pipeline_observations
    FOR EACH ROW EXECUTE FUNCTION judgment_check_evidence();

-- +goose Down
-- Destroys judgment and accounting history; no consumer output is reverted.
DROP TRIGGER judgment_delete_evidence ON l0_deletions;
DROP FUNCTION judgment_delete_evidence();
DROP TABLE pipeline_observations;
DROP TABLE judgment_journal;
DROP FUNCTION judgment_check_evidence();
DROP TRIGGER judgment_inputs ON l0_deletions;
DROP TRIGGER judgment_inputs ON l2_topic_operations;
DROP TRIGGER judgment_inputs ON l2_stances;
DROP TRIGGER judgment_inputs ON l2_topics;
DROP TRIGGER judgment_inputs ON l1_docs;
DROP TRIGGER judgment_inputs ON l0_events;
DROP FUNCTION judgment_guard_inputs();
DROP TABLE judgment_input_changes;
DROP TABLE judgment_input_guard;
