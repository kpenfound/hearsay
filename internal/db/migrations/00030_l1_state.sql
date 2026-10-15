-- +goose Up
ALTER TABLE l1_docs
    ADD COLUMN state text NULL CHECK (state IN ('proposed', 'started', 'blocked', 'completed', 'abandoned')),
    ADD COLUMN state_native text NULL;
CREATE INDEX l1_docs_state_idx ON l1_docs USING btree (state) WHERE state IS NOT NULL;

-- +goose Down
DROP INDEX l1_docs_state_idx;
ALTER TABLE l1_docs DROP COLUMN state_native, DROP COLUMN state;
