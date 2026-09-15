# 13. ACL re-syncs are durable and driven by the runtime

- Status: accepted
- Date: 2026-09-15

## Context

When a GitHub repository goes private, every artifact of it in L0 still carries
`acl: public`, and the connector re-emits each one under `perm:private`
([connector contract](../connector-contract.md), "When only the access list
changes"). The GitHub connector did that walk in a goroutine started from the
`repository.privatized` webhook, with its position on that goroutine's stack.
Two things lost it for good (issue #75):

- the process restarting partway through the walk, and
- the delivery never being handled: the service down when GitHub sent it, or a
  handler that failed. GitHub does not redeliver on its own.

Either way the rest of the repository stayed public in L0 and nothing knew a
re-sync was owed. The contract already said a re-sync is "resumable from a
cursor"; nothing kept one. The only durable position store, the backfill cursor,
is one walk per source that is never repeated once done.

## Decision

A re-sync is a runtime responsibility with its own durable record, and it is
found two ways.

- **`Resyncer`** is an optional connector interface: `Resync(ctx, sink,
  container, from Cursor)` does one bounded piece of the walk and returns what
  `Backfill` returns, and `Public(ctx, container)` says whether the source has
  the container as public now. The runtime drives `Resync` one call at a time
  and stores the cursor after each, as it does a backfill. The connector keeps
  no goroutine and no position.
- **`l0_resyncs`** holds one row per container of a source: `owed`, `cursor`,
  `generation` and `resynced_at`. Every write is one statement.
- **A push records before it answers.** The sink the runtime hands a connector
  implements `ResyncRequester`; the GitHub handler calls `RequestResync` on
  `privatized` and answers 202 only once the row is written, or 500 when it
  cannot be. A request resets the cursor and bumps the generation; a walk saves
  its cursor and settles the debt only while the generation is the one it read,
  so a walk that began before a request, possibly while the repository was
  public again, never settles it.
- **Startup finds what no delivery reported.** Alongside the owed re-syncs, the
  runtime reads which containers L0 serves as public (`l0.Store.Exposed`: each
  artifact's current revision, past retractions), asks `Public` about each one
  config still allows, and owes a re-sync for each the source calls private. A
  container whose last re-sync finished after its newest public artifact arrived
  is not asked about: whatever is still public there is something that walk could
  not reach, such as a commit a force push removed, and asking would walk the
  container again on every start.

## Alternatives considered

- **Extend the backfill cursor.** `l0_backfill_cursors` is one row per source
  and one walk that ends. A re-sync is per container and can be owed any number
  of times, so it would have needed a second shape in the same column, and it
  still would not cover a delivery that never arrived.
- **Only the startup check.** It catches both loss modes on its own, but only on
  the next start: a delivery handled by a running process would wait for a
  deploy, and a walk interrupted at its last page would start from the first.
- **A periodic visibility poll instead of a startup check.** It would catch a
  missed delivery without a restart, at one API call per repository per period
  for a change that happens rarely and is also delivered. Startup is where a
  missed delivery comes from, so that is where the check runs.
- **Keep the walk in the connector and give it a store.** Every connector that
  can change visibility would re-implement retry, backoff, health and storage,
  which the runtime already does for backfills.

## Consequences

- A container can be walked twice when a request arrives mid-walk or two
  replicas walk it at once. Both re-emit events L0 already has, which write
  nothing.
- Each start reads every current revision of each resyncing source once, and
  makes one `Public` call per container L0 serves as public. A repository that
  is still public costs a call on every start.
- A container the source will not answer about (a deleted repository) is
  retried with backoff and shows in `resync_failures` on `/readyz` for as long as
  L0 serves it as public. Each pass tries every owed container
  and the check, and a pass where anything worked is not followed by a backoff,
  so one failing container holds up neither the others nor the check.
- `repository.publicized` still re-syncs nothing: that direction fails closed.
  Tombstones keep the ACL they were emitted with.
