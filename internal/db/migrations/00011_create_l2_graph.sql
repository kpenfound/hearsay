-- +goose Up

-- The minimal L2 graph (docs/design.md#l2-the-graph): entities, topics, stances,
-- and the record of which L1 documents the assertion worker has read.
--
-- Unlike L1 this is not a cache of anything. A stance is a record — it is never
-- overwritten, only superseded — and human corrections will land in these
-- tables as first-class rows, so losing them loses what the team decided and
-- when.

-- Anything documents are about. Seeded from `code/` configuration, CODEOWNERS
-- and the top level of a repository at the assertion worker's startup, and
-- `tracker_item` entities on first reference from a document.
CREATE TABLE l2_entities (
    -- `code:acme/api:engine/server`, `tracker:github-acme:acme/api#12`.
    id text PRIMARY KEY,
    type text NOT NULL,
    -- What a person calls it, for display. Matched by resolve like an alias.
    name text NOT NULL DEFAULT '',
    aliases text[] NOT NULL DEFAULT '{}',
    -- Code entities only; resolved against HEAD at read time, never expanded
    -- into file lists here.
    path_patterns text[] NOT NULL DEFAULT '{}',
    -- The hierarchy, as edges to parent entity ids. Walked with a recursive CTE
    -- by the L3 views that inherit stances from ancestors. Deliberately not a
    -- foreign key: a `code/` entry may name a parent before it exists.
    part_of text[] NOT NULL DEFAULT '{}',
    -- Principal ids.
    owners text[] NOT NULL DEFAULT '{}',
    -- Where the entity came from. A re-seed replaces what it seeded and leaves
    -- the rest alone.
    origin text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l2_entities_type_is_known CHECK (
        type IN ('module', 'service', 'symbol', 'tracker_item', 'project', 'person', 'team')
    ),
    CONSTRAINT l2_entities_origin_is_known CHECK (
        origin IN ('config', 'repo_structure', 'reference')
    ),
    CONSTRAINT l2_entities_id_is_not_empty CHECK (id <> '')
);

-- A question the team has taken positions on.
CREATE TABLE l2_topics (
    id text PRIMARY KEY,
    -- The serial key the assertion worker ran under when it opened the topic:
    -- a configured scope id, or `source:<source id>` for a document no scope
    -- covers. Topics are only ever matched within one, which is what makes
    -- per-scope serialization sufficient (ADR-0007).
    scope text NOT NULL,
    name text NOT NULL,
    -- Entity ids the topic is about.
    about text[] NOT NULL DEFAULT '{}',
    -- The join keys of every document that took a position on it, which is
    -- what reference overlap matches a new document against.
    join_keys text[] NOT NULL DEFAULT '{}',
    -- Who may read the topic: the access list of the document that opened it.
    -- A later document is only offered this topic when everyone who may read
    -- that document may read this (docs/design.md#access-control).
    acl jsonb NOT NULL,
    -- The document that opened it.
    opened_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l2_topics_name_fits CHECK (name <> '' AND octet_length(name) <= 1000),
    CONSTRAINT l2_topics_scope_is_not_empty CHECK (scope <> ''),
    CONSTRAINT l2_topics_acl_is_not_empty CHECK (
        jsonb_typeof(acl) = 'array' AND jsonb_array_length(acl) > 0
    )
);

CREATE INDEX l2_topics_join_keys_idx ON l2_topics USING gin (join_keys);
CREATE INDEX l2_topics_scope_idx ON l2_topics (scope);

-- One position, from one source, at one time.
CREATE TABLE l2_stances (
    -- Derived from the topic, the first evidence and the position
    -- (l2.StanceID), so the primary key is what holds one document to one
    -- stance per position per topic, and what makes re-reading a document
    -- write nothing twice.
    id text PRIMARY KEY,
    topic_id text NOT NULL REFERENCES l2_topics (id),
    position text NOT NULL,
    -- A principal id, or empty where the document's author did not resolve: no
    -- placeholder principal is minted (internal/principal).
    author text NOT NULL DEFAULT '',
    -- The evidence's time, not the time the worker ran: supersession follows
    -- the order things were said in.
    stated_at timestamptz NOT NULL,
    -- L1 document ids. The first is the document the stance was read from.
    evidence text[] NOT NULL,
    -- The stance this one replaces on the topic. A stance is never overwritten:
    -- a change of position is a new row pointing at the old one.
    supersedes text REFERENCES l2_stances (id),
    tier text NOT NULL,
    -- The most restrictive access list of its evidence (docs/design.md#access-control).
    acl jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l2_stances_tier_is_known CHECK (tier IN ('ratified', 'inferred', 'contested')),
    CONSTRAINT l2_stances_position_fits CHECK (position <> '' AND octet_length(position) <= 2000),
    CONSTRAINT l2_stances_has_evidence CHECK (cardinality(evidence) > 0),
    CONSTRAINT l2_stances_acl_is_not_empty CHECK (
        jsonb_typeof(acl) = 'array' AND jsonb_array_length(acl) > 0
    ),
    CONSTRAINT l2_stances_does_not_supersede_itself CHECK (supersedes IS DISTINCT FROM id)
);

CREATE INDEX l2_stances_topic_idx ON l2_stances (topic_id, stated_at DESC);
CREATE INDEX l2_stances_evidence_idx ON l2_stances USING gin (evidence);

-- Which version of each L1 document the assertion worker has read. The
-- version is the document's distilled_at, which l1.Store.Put leaves alone when a
-- re-distillation changes nothing; it is compared for equality and never
-- ordered, because two writers' now() say nothing about which committed last.
CREATE TABLE l2_asserted (
    doc_id text PRIMARY KEY,
    distilled_at timestamptz NOT NULL,
    stances integer NOT NULL DEFAULT 0,
    asserted_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l2_asserted_stances_is_not_negative CHECK (stances >= 0)
);

-- +goose Down

-- Destroys every stance and topic, which are records rather than derived rows:
-- re-running the assertion worker produces a graph, not this one.
-- `hearsay migrate down` refuses without --i-know (ADR-0006).
DROP TABLE l2_asserted;
DROP TABLE l2_stances;
DROP TABLE l2_topics;
DROP TABLE l2_entities;
