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
  rather than reading past it. It takes the same `Filter` a listing does — a
  cursor is a position in the whole feed, so the filter narrows what comes back
  and not where a reader is.

An artifact's history comes back oldest first by default, which is
[the connector contract](../../docs/connector-contract.md)'s ordering of
revisions **reversed**. `ListOptions.Newest` is the contract's own direction, and
that is the one to ask for when you want the *current* revision: an ACL re-sync
is a new revision, so the first result of the default order carries the access
list the re-sync replaced.

`Current` is the read everything above L0 actually wants: one row per artifact,
and the row is the revision that is current by the contract's order. A listing
hands back a history, which is what provenance needs and what distillation does
not — and doing the fold in one statement is also what keeps a limit meaningful,
because the limit counts artifacts there and revisions in a listing.

`Filter.Thread` reads one conversation: every event that hangs off an artifact,
which is what an L1 document is assembled from. It matches the contract's own
rule — an event's `thread` is the root of the conversation, and its `parent` is
that root on a source with no threads — so it is one predicate either sort of
source answers, and it is indexed as the same expression.

`Cursors` is where a consumer of the change feed keeps its position, one row per
consumer rather than per replica. It lives in the database because a consumer is
a stateless process that gets restarted: a distiller that lost its position would
re-distil everything ever ingested. A save never moves a cursor backwards, so two
replicas of one consumer cannot make the feed be read again; resetting one is
deleting its row.

**Does not belong here:** anything that interprets an event. Distillation is
`internal/l1`, and no code in this package calls a model. Connectors — the code
that produces events — live in `internal/connector`, and so does the event type
itself: the event shape is the connector contract, and this package stores it.

See [docs/design.md](../../docs/design.md#l0-events) and
[ADR-0004](../../docs/adr/0004-one-postgres-for-all-four-layers.md).
