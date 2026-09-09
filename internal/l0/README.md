# internal/l0

The event layer: what happened, as the source reported it.

**Belongs here:** the append-only store, ingest (idempotent on the event id,
which is derived from `source` + `native_id`), tombstones, the change feed, and
reading an event back by id for a provenance walk.

`Store` implements `connector.Sink`, so a connector's `Emit` writes here. Three
things about it are worth knowing before you use it:

- **Nothing is ever updated.** An edit arrives as a new event, because the
  revision token is part of the native id and so part of the derived event id.
  Re-emitting an unchanged event writes nothing; re-emitting an id with
  different content is a connector breaking the contract, and returns
  `ErrRewrite` rather than overwriting the row.
- **A deletion is a tombstone.** A tombstone event names the artifact it
  retracts, and from then on every read excludes every event of that artifact —
  `Get` says `ErrRetracted`, `List` and `Changes` leave it out, `Counts` shows
  the row still there. The tombstone itself stays on the feed, which is how a
  consumer learns to walk provenance forward.
- **The change feed never skips.** `Changes` hands out an event only once the
  transaction that wrote it has finished, and cursors move forward through
  finished transactions only, so a reader that stops and resumes misses nothing.
  The price is that the feed waits behind a write transaction that is still open
  rather than reading past it.

**Does not belong here:** anything that interprets an event. Distillation is
`internal/l1`, and no code in this package calls a model. Connectors — the code
that produces events — live in `internal/connector`, and so does the event type
itself: the event shape is the connector contract, and this package stores it.

See [docs/design.md](../../docs/design.md#l0-events) and
[ADR-0004](../../docs/adr/0004-one-postgres-for-all-four-layers.md).
