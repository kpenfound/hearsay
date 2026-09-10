-- +goose Up

-- The L1 document table: one row per artifact, the common envelope from
-- docs/design.md#l1-distilled-documents plus a per-kind body.
--
-- L1 is derived. Every row can be rebuilt from the events l0_refs names, and
-- the distiller rewrites a row rather than appending: nothing here is a record
-- of anything, and losing the table costs model calls rather than data.
CREATE TABLE l1_docs (
    -- `l1:<source>:<artifact>`, derived from the two by l1.DocID. Making it the
    -- primary key is what makes re-distilling an artifact idempotent: the same
    -- artifact always writes the same row.
    id text PRIMARY KEY,

    -- The document kind (`issue`, `pr`, `commit`), which is L1's own
    -- vocabulary rather than L0's: several L0 kinds make one document. It is
    -- deliberately not constrained to a list — the vocabulary grows with every
    -- source, and a new one would otherwise be a migration.
    kind text NOT NULL,

    -- The artifact this document distils: the configured source instance, the
    -- artifact id within it, and the permalink.
    source text NOT NULL,
    source_native_id text NOT NULL,
    source_url text NOT NULL DEFAULT '',

    -- Provenance, required: the event ids the document was built from, in the
    -- order the document reads. Every line served to a consumer can be followed
    -- back through this, and a deletion walks it forward
    -- (docs/design.md#deletion-and-provenance).
    l0_refs text[] NOT NULL,

    -- The artifact's own times. created never moves — every revision of an
    -- artifact carries it — updated is the newest revision's edit time, and
    -- last_activity is the newest of anything in the document, which is what
    -- "recent activity on a scope" is ordered by.
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    last_activity_at timestamptz NOT NULL,

    -- [{principal_id, role}], resolved through the identity mapping. An
    -- identity that does not resolve is not here: no placeholder principal id
    -- is minted (internal/principal).
    participants jsonb NOT NULL DEFAULT '[]'::jsonb,

    -- Entity ids this document is about: tracker items, code entities,
    -- projects. Scopes filter for relevance and never grant permission — that
    -- is acl, and they are never the same mechanism.
    scope text[] NOT NULL DEFAULT '{}',

    -- [{type, id}], extracted deterministically before any model call and never
    -- by the model. It is the join key L2 matches topics on.
    --
    -- The column is `refs` because `references` is a reserved word in SQL, the
    -- same reason L0's `time` is `occurred_at`.
    refs jsonb NOT NULL DEFAULT '[]'::jsonb,

    -- Who may read the document, inherited from the events it is built from and
    -- re-synced whenever they change: an access list changing at the source is
    -- a new revision of the artifact, so a re-sync arrives as a re-distillation
    -- with no second mechanism (docs/design.md#access-control).
    acl jsonb NOT NULL,

    -- text is the distillation and is what gets embedded; raw_text is the
    -- team's own words and is full-text indexed only, never embedded. Both are
    -- scrubbed of secrets and personal data before they are written. The
    -- embedding column and the full-text index arrive with search (#50).
    text text NOT NULL,
    raw_text text NOT NULL,

    -- The per-kind body: summary, question, outcome, open questions and what a
    -- change proposal does.
    body jsonb NOT NULL,

    -- Lifted out of body because reads index on it: outcome_kind is the L2
    -- trigger, and only `decided`, `proposed` and `resolved` enter the
    -- assertion pipeline. The five values are the design's and are closed, so
    -- they are a constraint here rather than a sentence in a doc comment.
    outcome_kind text NOT NULL,

    -- When this row was last written. It does not move when a re-distillation
    -- produces the document that is already there, which is what makes
    -- "distilling twice produces identical rows" checkable.
    distilled_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT l1_docs_outcome_kind_is_known CHECK (
        outcome_kind IN ('resolved', 'decided', 'proposed', 'open', 'none')
    ),
    -- A document that names no events could not be regenerated or followed
    -- back, which is the whole of what L1 promises.
    CONSTRAINT l1_docs_has_provenance CHECK (cardinality(l0_refs) > 0),
    -- A document nobody may read cannot be read back, so an empty access list
    -- is a bug rather than a private document.
    CONSTRAINT l1_docs_acl_is_not_empty CHECK (
        jsonb_typeof(acl) = 'array' AND jsonb_array_length(acl) > 0
    ),
    CONSTRAINT l1_docs_json_columns_are_arrays CHECK (
        jsonb_typeof(participants) = 'array' AND jsonb_typeof(refs) = 'array'
    ),
    CONSTRAINT l1_docs_body_is_an_object CHECK (jsonb_typeof(body) = 'object'),
    -- A revision cannot precede the thing it revises, and neither can a comment
    -- on it.
    CONSTRAINT l1_docs_times_are_ordered CHECK (
        updated_at >= created_at AND last_activity_at >= created_at
    ),
    -- The id is derived from the source and the artifact. Stating it here means
    -- a row written by anything other than l1.Store still parses back.
    CONSTRAINT l1_docs_id_is_derived CHECK (
        id = 'l1:' || source || ':' || source_native_id
    )
);

-- Listing a source's documents, newest activity first.
CREATE INDEX l1_docs_source_kind_idx ON l1_docs (source, kind, last_activity_at DESC);

-- The assertion worker's read: the documents that concluded something
-- (docs/design.md#l2-write-path). Partial, because that is the only value of
-- this column anything looks up by, and it keeps the index the size of the
-- documents L2 cares about rather than of the table.
CREATE INDEX l1_docs_asserting_idx ON l1_docs (outcome_kind, last_activity_at DESC)
    WHERE outcome_kind IN ('decided', 'proposed', 'resolved');

-- Scope filtering, which every read on the path to a bundle does.
CREATE INDEX l1_docs_scope_idx ON l1_docs USING gin (scope);

-- Reference overlap, which is how L2 matches a document to a topic before it
-- reaches for an embedding. jsonb_path_ops is the smaller index and supports
-- the containment operator, which is the only one that query uses.
CREATE INDEX l1_docs_refs_idx ON l1_docs USING gin (refs jsonb_path_ops);

-- +goose Down

-- L1 is derived, so this loses no record — but re-deriving it is a model call
-- per document, which is not free. `hearsay migrate down` refuses without
-- --i-know (ADR-0006).
DROP TABLE l1_docs;
