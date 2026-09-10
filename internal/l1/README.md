# internal/l1

The document layer: what was said. One document per L0 artifact, flat rows in
one schema, rebuilt from L0 whenever the distillation changes.

**Belongs here:** the envelope and the per-kind bodies, the store, deterministic
reference extraction, the secrets and PII scrub that runs before `text` is
written, and hybrid search over the table (embeddings plus full text).

**Does not belong here:** the prompts that produce a document — those belong to
the distiller in `internal/service/distiller`, which owns its own prompt
(ADR-0005). Entities, topics and stances are `internal/l2`.

Distillation is a write-time LLM call. Reads never call a model.

## Things to know before changing it

- **A document is a function of its events.** [Build] takes the current revision
  of an artifact and of everything that hangs off it and produces everything but
  the body; the body is the one part that takes a model call. Nothing else may
  reach into a document — not a clock, not a map iteration order — or
  re-distilling would rewrite a row that has not changed, which is the property
  the acceptance test pins.
- **Only current revisions.** An artifact's earlier revisions are in L0 and are
  what a provenance walk reads. An access-list re-sync is a revision like any
  other, so building from anything but the newest would inherit an access list
  that has been replaced.
- **The access list is folded, not inherited.** A document quotes every event in
  it, so it carries the grants every one of them carries. A reply that shares no
  grant with the artifact is left out of the document rather than narrowing it to
  nothing: a document with an empty access list is unreadable for ever, which is
  not the same thing as a private one.
- **Nothing unresolved is invented.** An @-mention or an author that the identity
  mapping does not cover produces no participant and no reference; the resolver
  keeps the sighting for a person to map (`internal/principal`). Fixing the
  mapping and re-distilling is what recovers authorship.
- **`text` is embedded, `raw_text` is not.** `text` is the distillation — the
  model's words, nothing else — because a vector over one document about one
  thing is what a similarity search is for. `raw_text` is the team's own words,
  for the full-text index (#50).
- **The scrub runs over both, over the body, and over a `url` reference.** Those
  are the four strings a row holds that a person wrote; the other reference ids
  are structured, and a marker in one would break the join it exists for. It is
  idempotent by construction: every replacement is a marker no pattern matches
  the inside of, and there is a test that says so. It is a net rather than a
  proof, and it is the last of the four access-control control points, not the
  first (docs/design.md#access-control).
- **A reference is never read out of the inside of a link.** A URL contains
  every shape extraction looks for — `#31` in a path, `@name` in a userinfo or a
  path, an entity name in a directory — so links are matched first and the rest
  of the text is read with them blanked out. An `@`-mention also has to start a
  word, or it fires on the domain of an email address.
- **`outcome_kind` is the L2 trigger and is on every document.** The five values
  are the design's, the column carries a CHECK over exactly them, and
  `OutcomeKind.Asserts` is the one place that says which of them the assertion
  pipeline reads.
- **The column is `refs`, not `references`,** because `references` is reserved in
  SQL — the same reason L0's `time` is `occurred_at`.

See [docs/design.md](../../docs/design.md#l1-distilled-documents).
