-- +goose Up

-- The topic ledger (issue #171, feature #26): every merge, split and undo a
-- person made in a scope, appended and never changed. An operation touches no
-- topic or stance row; what it makes of them is read from the ledger, so an
-- undo is another row and the rows it reverses stay as they were.

-- id orders a scope's operations: they are recorded one at a time under the
-- scope's serial key, so id order is the order they were decided in.
CREATE TABLE l2_topic_operations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('merge', 'split', 'undo')),
    -- The serial key the topics belong to (l2_topics.scope).
    scope text NOT NULL CHECK (scope <> ''),
    -- The configured human who asked.
    principal text NOT NULL CHECK (principal <> ''),
    -- What it covers. A merge's topics are [into, from] and its stances every
    -- stance on `from` when it merged; a split's are [topic, new topic] and
    -- the stances it moved; an undo repeats those of the operation it undoes.
    topics text[] NOT NULL,
    stances text[] NOT NULL,
    -- A split's new topic name. The new topic has no row in l2_topics.
    name text,
    undoes bigint REFERENCES l2_topic_operations (id),
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l2_topic_operations_two_topics CHECK (
        cardinality(topics) = 2 AND topics[1] <> topics[2]
    ),
    CONSTRAINT l2_topic_operations_shape CHECK (
        CASE kind
            WHEN 'merge' THEN name IS NULL AND undoes IS NULL
            WHEN 'split' THEN name IS NOT NULL AND name <> '' AND cardinality(stances) > 0 AND undoes IS NULL
            ELSE name IS NULL AND undoes IS NOT NULL
        END
    )
);

-- An operation is undone at most once.
CREATE UNIQUE INDEX l2_topic_operations_undoes_idx ON l2_topic_operations (undoes);
CREATE INDEX l2_topic_operations_scope_idx ON l2_topic_operations (scope, created_at);
CREATE INDEX l2_topic_operations_kind_idx ON l2_topic_operations (kind, created_at);
CREATE INDEX l2_topic_operations_topics_idx ON l2_topic_operations USING gin (topics);

-- +goose Down

-- Destroys the record of every correction a person made to the graph, and
-- with it what those corrections made of the topics. `hearsay migrate down`
-- refuses without --i-know (ADR-0006).
DROP TABLE l2_topic_operations;
