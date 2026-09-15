# internal/l3

Derived views: what is true right now. Rebuilt from L2 on demand, never written
directly.

**Belongs here:** current stances for an entity (including ones inherited from
ancestors, tagged as inherited), recent L1 activity on a scope, open questions,
and ownership derived from participation and authorship. Caching and
materialization of these views, if it becomes worth it.

**Does not belong here:** any write path. If something cannot be recomputed from
L2 it is not an L3 view — it is an L2 fact and belongs in `internal/l2`.

## Things to know before changing it

- **Every view is for a reader.** The access lists are applied before a count
  cuts anything (`l1.Store.ListFor`), so a document somebody may not read never
  takes one of their places in `recent`.
- **A topic's current stance is the newest stated that is not retired,** the
  head of its supersession chain; a stance its own document's later reading
  replaced is retired (internal/l2). When the reader may not read it, the topic is left out and counted —
  the older stance they may read is not current, and offering it as current
  would be wrong in a way they could not see.
- **A stance is an entity's own only when its topic's `about` names that entity
  itself.** Every other topic reached — through a related entity (for a tracker
  item, the code entities its own document is about) or through an ancestor of
  either — is inherited, and comes after the entity's own. The line is there
  because a document's scope is broad: every document in a repository is about
  the repository's code entity, and so is every topic one opened, so "about any
  entity the item's document is about" would make every topic in the repository
  a stance of every item. The walk up `part_of` is a recursive `UNION`, so a
  cycle ends it.
- **Ownership and "who knows X" are not built yet.**

See [docs/design.md](../../docs/design.md#l3-derived-views).
