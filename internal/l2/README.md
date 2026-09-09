# internal/l2

The graph: what it means. Entities, topics, stances, anchors and the edges
between them.

**Belongs here:** the entity hierarchy and its aliases, topics, stances with
their tier and evidence, supersession chains, anchors, and the recursive
queries over them. Human corrections — ratify, demote, pin, merge — land here
too, and are as first-class as anything the assertion worker wrote.

**Does not belong here:** the extraction prompts (the assertion worker owns
them), and anything derived that can be recomputed — that is `internal/l3`.

Two invariants to preserve: a stance is never overwritten, only superseded; and
one scope's writes are serialized, or supersession chains interleave and
corrupt.

See [docs/design.md](../../docs/design.md#l2-the-graph).
