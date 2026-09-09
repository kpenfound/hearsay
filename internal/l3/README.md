# internal/l3

Derived views: what is true right now. Rebuilt from L2 on demand, never written
directly.

**Belongs here:** current stances for an entity (including ones inherited from
ancestors, tagged as inherited), recent L1 activity on a scope, open questions,
and ownership derived from participation and authorship. Caching and
materialization of these views, if it becomes worth it.

**Does not belong here:** any write path. If something cannot be recomputed from
L2 it is not an L3 view — it is an L2 fact and belongs in `internal/l2`.

See [docs/design.md](../../docs/design.md#l3-derived-views).
