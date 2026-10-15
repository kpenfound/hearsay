-- +goose Up
CREATE INDEX l2_topics_about_idx ON l2_topics USING gin (about);
CREATE INDEX l2_entities_part_of_idx ON l2_entities USING gin (part_of);

-- +goose Down
DROP INDEX l2_topics_about_idx;
DROP INDEX l2_entities_part_of_idx;
