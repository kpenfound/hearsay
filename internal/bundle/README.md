# internal/bundle

Assembly of the context bundle — the primary read, and the thing the whole
system exists to produce.

**Belongs here:** section assembly (directive, scope, anchors, stances, recent,
open questions, conflicts, handles), the token budget and the drop order,
dedupe by kind before cutting by recency, conflict computation, and the
permission filtering that decides what a given principal sees.

**Does not belong here:** transport. Serving the bundle over MCP or HTTP is
`internal/service/api`. No model is called on this path — a bundle is a
structured lookup, and query-time synthesis is a non-goal.

Rules worth restating because they are easy to break: `recent` is capped by
count and never filtered by age; ratified stances never drop; `anchors` are not
subject to the `recent` cap; no code content, ever.

## Things to know before changing it

- **A scope is an entity id** — `tracker:github:acme/api#12`, `code:acme/api`. The
  entities a bundle is about are that one and the ones its own document is
  about; the ancestors of those are where inherited stances come from.
- **Every time is absolute.** The design writes `when: 23d`; a relative age would
  make the same scope and principal produce a different bundle every minute,
  which breaks "cacheable". A consumer computes the age.
- **The budget is estimated from bytes** ([BytesPerToken]), not a tokenizer: the
  bundle must not depend on which model a deployment names. It drops from the
  bottom — open questions, then `recent`, then stances that are not ratified,
  last first — and can end over budget, because ratified stances never drop.
- **[Encode] is the one encoding.** Every interface serves its bytes verbatim;
  that is what "identical output regardless of which interface asked" rests on.
- **Only a line with an id.** An entity has a `line` only when it is a document
  the reader may read, and then it carries that document's `l1`.
- **Not built yet:** the directive, anchors (always `[]`) and conflicts
  (always `[]`).

See [docs/design.md](../../docs/design.md#the-context-bundle).
