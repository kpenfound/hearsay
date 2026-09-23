-- +goose Up

-- A bundle revision is the count of committed changes in this table. Using
-- the identity value alone would be unsafe: a lower value can commit after a
-- higher one. Inserts take no shared row lock, so a slow L0 transaction does
-- not stall unrelated writers. Audit events leave this table unchanged.
CREATE TABLE bundle_changes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY
);

-- +goose StatementBegin
CREATE FUNCTION record_bundle_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'l0_events' THEN
        IF NEW.kind = 'audit' THEN
            RETURN NEW;
        END IF;
    END IF;
    INSERT INTO bundle_changes DEFAULT VALUES;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER bundle_l0_watermark AFTER INSERT ON l0_events
    FOR EACH ROW EXECUTE FUNCTION record_bundle_change();
CREATE TRIGGER bundle_l1_watermark AFTER INSERT OR UPDATE OR DELETE ON l1_docs
    FOR EACH ROW EXECUTE FUNCTION record_bundle_change();
CREATE TRIGGER bundle_l2_entities_watermark AFTER INSERT OR UPDATE OR DELETE ON l2_entities
    FOR EACH ROW EXECUTE FUNCTION record_bundle_change();
CREATE TRIGGER bundle_l2_topics_watermark AFTER INSERT OR UPDATE OR DELETE ON l2_topics
    FOR EACH ROW EXECUTE FUNCTION record_bundle_change();
CREATE TRIGGER bundle_l2_stances_watermark AFTER INSERT OR UPDATE OR DELETE ON l2_stances
    FOR EACH ROW EXECUTE FUNCTION record_bundle_change();
CREATE TRIGGER bundle_l2_pins_watermark AFTER INSERT OR UPDATE OR DELETE ON l2_pins
    FOR EACH ROW EXECUTE FUNCTION record_bundle_change();

-- +goose Down
DROP TRIGGER bundle_l2_pins_watermark ON l2_pins;
DROP TRIGGER bundle_l2_stances_watermark ON l2_stances;
DROP TRIGGER bundle_l2_topics_watermark ON l2_topics;
DROP TRIGGER bundle_l2_entities_watermark ON l2_entities;
DROP TRIGGER bundle_l1_watermark ON l1_docs;
DROP TRIGGER bundle_l0_watermark ON l0_events;
DROP FUNCTION record_bundle_change();
DROP TABLE bundle_changes;
