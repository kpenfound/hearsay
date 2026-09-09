# internal/l0

The event layer: what happened, as the source reported it.

**Belongs here:** the event type and its `acl`, the append-only store, ingest
(idempotent on `source` + `native_id`), tombstones, and reading an event back by
id for a provenance walk.

**Does not belong here:** anything that interprets an event. Distillation is
`internal/l1`, and no code in this package calls a model. Connectors — the code
that produces events — live in `internal/connector`.

See [docs/design.md](../../docs/design.md#l0-events) and
[ADR-0004](../../docs/adr/0004-one-postgres-for-all-four-layers.md).
