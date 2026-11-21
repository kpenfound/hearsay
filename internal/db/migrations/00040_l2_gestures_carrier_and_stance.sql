-- +goose Up

-- A gesture can name one stance instead of documents, and can record the agent
-- that carried it.
--
-- carrier is the agent that carried a person's decision to the ledger: the
-- principal is still the human who made it and the only one authorized.
-- Reactions, chat and GitHub commands and the CLI record no carrier, so it is
-- NULL for every gesture written before this and for those still.
--
-- target_stance is set when a ratify or a demote named one stance, not the
-- documents it is drawn from. The row still records that stance's evidence
-- documents in `documents`, so l2_gestures_shape holds as it was written, and
-- `stances` is exactly the one stance, which is what every standing read and
-- the evaluation's time to ratification already read. An undo repeats
-- `documents` and `stances` and leaves target_stance NULL: what it undoes is
-- `undoes`.
ALTER TABLE l2_gestures ADD COLUMN carrier text CHECK (carrier <> '');
ALTER TABLE l2_gestures ADD COLUMN target_stance text CHECK (target_stance <> '');
ALTER TABLE l2_gestures ADD CONSTRAINT l2_gestures_target_stance_shape CHECK (
    target_stance IS NULL
    OR (action IN ('ratify', 'demote') AND stances = ARRAY[target_stance])
);

-- +goose Down

-- Loses which agent carried a decision, and which gestures named a single
-- stance; the stances they applied to stay in `stances`.
ALTER TABLE l2_gestures DROP CONSTRAINT l2_gestures_target_stance_shape;
ALTER TABLE l2_gestures DROP COLUMN target_stance;
ALTER TABLE l2_gestures DROP COLUMN carrier;
