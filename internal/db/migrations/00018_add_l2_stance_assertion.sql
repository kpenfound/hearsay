-- +goose Up

-- A stance an agent wrote through the API's `assert` call is read from an L0
-- `assertion` event rather than from an L1 document. Its evidence is the L1
-- documents the agent cited, none of which it was read from, so the event is
-- recorded beside them: it is what a stance's origin is where it is set, and
-- evidence[1] is where it is not.
ALTER TABLE l2_stances ADD COLUMN assertion text;
ALTER TABLE l2_stances ADD CONSTRAINT l2_stances_assertion_is_an_event
    CHECK (assertion LIKE 'evt:%');

-- One stance per assertion event, and the startup sweep's lookup of the events
-- that have none.
CREATE UNIQUE INDEX l2_stances_assertion_idx ON l2_stances (assertion) WHERE assertion IS NOT NULL;

-- +goose Down

-- Stances written from an assertion keep their evidence and lose the record of
-- the event they were read from: a later reading of a cited document would then
-- take them for its own.
DROP INDEX l2_stances_assertion_idx;
ALTER TABLE l2_stances DROP CONSTRAINT l2_stances_assertion_is_an_event;
ALTER TABLE l2_stances DROP COLUMN assertion;
