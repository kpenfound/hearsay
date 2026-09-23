-- +goose Up

-- The assertion worker judges a new stance against the current position it
-- was shown. Historical stances have no recorded comparison.
ALTER TABLE l2_stances ADD COLUMN judgement text;
ALTER TABLE l2_stances ADD CONSTRAINT l2_stances_judgement_is_known
    CHECK (judgement IN ('changes', 'restates'));

-- +goose Down

ALTER TABLE l2_stances DROP CONSTRAINT l2_stances_judgement_is_known;
ALTER TABLE l2_stances DROP COLUMN judgement;
