-- +goose Up

-- A withdrawal remains in history while the stance it supersedes stops
-- contributing to the current position. A surviving-evidence replacement of
-- an agent assertion retains its event origin, so several historical rows may
-- cite the same event.
ALTER TABLE l2_stances ADD COLUMN withdrawn boolean NOT NULL DEFAULT false;
DROP INDEX l2_stances_assertion_idx;
CREATE INDEX l2_stances_assertion_idx ON l2_stances (assertion) WHERE assertion IS NOT NULL;

-- +goose Down

DROP INDEX l2_stances_assertion_idx;
CREATE INDEX l2_stances_assertion_idx ON l2_stances (assertion) WHERE assertion IS NOT NULL;
ALTER TABLE l2_stances DROP COLUMN withdrawn;
