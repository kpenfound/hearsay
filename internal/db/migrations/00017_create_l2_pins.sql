-- +goose Up

-- A pin: a person saying that one L1 document is an anchor of a scope
-- (docs/design.md#anchors). Like a stance this is a record, not a cache —
-- nothing re-derives who pinned what — so it lives in L2 beside the corrections
-- people make to the graph.
--
-- The document is deliberately not a foreign key. L1 is derived: a document
-- retracted and distilled again, or rebuilt from L0, keeps its pin, and a pin
-- whose document is gone is simply not served.
CREATE TABLE l2_pins (
    -- The entity id the document anchors: `tracker:github-acme:acme/api#12`.
    scope text NOT NULL,
    -- The L1 document id.
    l1 text NOT NULL,
    -- The principal id of whoever pinned it.
    pinned_by text NOT NULL,
    pinned_at timestamptz NOT NULL,

    -- One pin per document per scope; the key's prefix is the read by scope.
    PRIMARY KEY (scope, l1),
    CONSTRAINT l2_pins_scope_is_not_empty CHECK (scope <> ''),
    CONSTRAINT l2_pins_l1_is_a_document CHECK (l1 LIKE 'l1:%'),
    CONSTRAINT l2_pins_pinned_by_is_not_empty CHECK (pinned_by <> '')
);

-- +goose Down

-- Destroys every pin, which people made and nothing re-derives.
-- `hearsay migrate down` refuses without --i-know (ADR-0006).
DROP TABLE l2_pins;
