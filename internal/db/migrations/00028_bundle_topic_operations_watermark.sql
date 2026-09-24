-- +goose Up

-- Topic operations change the effective graph without updating topic or
-- stance rows, so they must invalidate bundles through the shared watermark.
CREATE TRIGGER bundle_l2_topic_operations_watermark AFTER INSERT ON l2_topic_operations
    FOR EACH ROW EXECUTE FUNCTION record_bundle_change();

-- +goose Down
DROP TRIGGER bundle_l2_topic_operations_watermark ON l2_topic_operations;
