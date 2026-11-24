-- +goose Up

-- The published distillation of each conversation a conversation source holds:
-- exactly the message revisions it was built from, and the documents it made.
--
-- Job state and document lookups cannot say that a message was processed: the
-- queue records no inputs, and a distillation that quoted nothing leaves no
-- document behind. This row is written in the transaction that reconciles the
-- conversation's documents, by the attempt that read those revisions and no
-- other, so a message is covered only by a distillation that included its
-- current revision. A failed or superseded attempt writes nothing.
CREATE TABLE l1_conversation_distillations (
    source text NOT NULL,
    -- The conversation id the source's messages carry as their thread.
    thread text NOT NULL,

    -- The L0 event ids of the current revision of every message the
    -- distillation read, sorted. An event id names one revision, so a message
    -- rewritten since is not among them.
    inputs text[] NOT NULL,

    -- The ids of the documents it produced, sorted. Empty where segmentation
    -- selected nothing: the record is the distillation's result all the same.
    documents text[] NOT NULL,

    -- When this row last changed. A distillation that publishes the same inputs
    -- and documents again writes nothing and leaves it alone.
    published_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (source, thread)
);

-- Which distillation included a message revision, asked of event ids.
CREATE INDEX l1_conversation_distillations_inputs_idx ON l1_conversation_distillations USING gin (inputs);

-- +goose Down

-- The record is derived and published again by the next distillation of each
-- conversation, but until then every message reads as not yet distilled.
DROP TABLE l1_conversation_distillations;
