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

See [docs/design.md](../../docs/design.md#the-context-bundle).
