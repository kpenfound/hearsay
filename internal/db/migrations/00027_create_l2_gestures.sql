-- +goose Up

-- The gesture ledger (issue #179, feature #24): every ratify, demote, pin and
-- undo a person made by hand, appended and never changed. A ratification or a
-- demotion touches no stance row; what it does to a topic's standing is read
-- from the ledger (l2.Stand). A pin also writes l2_pins, in the same
-- transaction, and its undo takes the pin out again.

-- id orders the gestures: a scope's are recorded one at a time under its
-- serial key, so id order is the order they were decided in.
CREATE TABLE l2_gestures (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The L0 event the gesture came from: a reaction, a command. It is the
    -- idempotency key, so a retried event records nothing new, and it is what
    -- an operator deletion of that event voids.
    event text NOT NULL CHECK (event LIKE 'evt:%'),
    action text NOT NULL CHECK (action IN ('ratify', 'demote', 'pin', 'undo')),
    -- The serial key the gesture was recorded under (l2_topics.scope).
    scope text NOT NULL CHECK (scope <> ''),
    -- The configured human who made it.
    principal text NOT NULL CHECK (principal <> ''),
    -- What it covers: the L1 documents the person pointed at; the live stances
    -- drawn from them that a ratify or a demote applied to; the pins a pin
    -- made, as [{"scope": entity id, "l1": document id}]. An undo repeats
    -- those of the gesture it undoes.
    documents text[] NOT NULL,
    stances text[] NOT NULL,
    pins jsonb NOT NULL DEFAULT '[]',
    undoes bigint REFERENCES l2_gestures (id),
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l2_gestures_shape CHECK (
        CASE action
            WHEN 'pin' THEN undoes IS NULL AND cardinality(documents) > 0
                AND cardinality(stances) = 0 AND jsonb_array_length(pins) > 0
            WHEN 'undo' THEN undoes IS NOT NULL
            ELSE undoes IS NULL AND cardinality(documents) > 0
                AND cardinality(stances) > 0 AND pins = '[]'
        END
    )
);

-- One gesture per source event, and a gesture is undone at most once.
CREATE UNIQUE INDEX l2_gestures_event_idx ON l2_gestures (event);
CREATE UNIQUE INDEX l2_gestures_undoes_idx ON l2_gestures (undoes);
-- Every standing read asks which gestures name its stances.
CREATE INDEX l2_gestures_stances_idx ON l2_gestures USING gin (stances);
CREATE INDEX l2_gestures_scope_idx ON l2_gestures (scope, created_at);

-- A gesture changes what a bundle serves, so it moves the bundle watermark
-- (00020) as a stance does.
CREATE TRIGGER bundle_l2_gestures_watermark AFTER INSERT ON l2_gestures
    FOR EACH ROW EXECUTE FUNCTION record_bundle_change();

-- +goose Down

-- Destroys the record of every ratification, demotion and pin a person made
-- by hand, and with it what they did to the topics' standing. The pins stay in
-- l2_pins. `hearsay migrate down` refuses without --i-know (ADR-0006).
DROP TABLE l2_gestures;
