# 7. Postgres-backed job queue with per-key serialization

- Status: accepted
- Date: 2026-09-08

## Context

Two of the four services (ADR-0003) are workers, and they want different things from a
queue.

- **The distiller** turns L0 events into L1 documents. High volume, wide parallelism,
  stateless and idempotent by design: any L1 doc can be regenerated from its `l0_refs`.
  It calls a model per job, so jobs are slow (seconds) and can fail transiently.
- **The assertion worker** turns L1 documents into L2 stances. Low volume, and
  `docs/design.md` requires it to be **serialized per scope**, because two concurrent
  jobs on the same scope can both decide to open a topic for the same question and
  produce exactly the wrong-merge failure the design says must be rare.

Both are triggered by a row appearing in the database: a connector writes an L0 event, or
the distiller writes an L1 doc whose `outcome_kind` qualifies. So enqueueing and the
write that causes it are naturally in one transaction, and losing one without the other
is a silent gap in the pipeline.

Everything else already lives in one Postgres (ADR-0004), and Hearsay is self-hosted, so
every extra stateful service is something an operator must run and back up.

## Decision

A Postgres-backed job queue in `internal/queue`, owned by Hearsay's own schema and
migrations (ADR-0006). No external broker.

**Enqueue is transactional.** A job row is inserted in the same transaction as the L0 or
L1 write that causes it. Either both happen or neither does, and there is no outbox to
reconcile.

Because the enqueue shares the caller's transaction, it must never be able to abort it.
Job identity is a partial unique index on `(kind, target_id) WHERE state = 'pending'`, and
a plain `INSERT` against it would raise a unique violation and roll back the L0 or L1
write that was the point of the transaction. The enqueue is therefore always:

```sql
INSERT INTO queue_job (kind, target_id, serial_key, priority, run_after, trace_context)
VALUES (...)
ON CONFLICT (kind, target_id) WHERE state = 'pending' DO NOTHING;
```

The `WHERE` clause is required for Postgres to infer a partial index. This is the clause
that makes a duplicate enqueue collapse instead of failing: the deletion walk in
`docs/design.md` re-distills an affected L1 doc and enqueues an assert job for it, and an
assert job for that `l1_id` may already be pending from the original distillation. With a
plain `INSERT`, deletion fails on a queue constraint.

**`serial_key` is the reason this is hand-written.** A job with a `serial_key` never runs
while another job with the same key is running. The assertion worker sets
`serial_key = scope`, which is exactly the serialization the design asks for, while
leaving different scopes fully parallel. The distiller leaves it null and runs as wide as
its concurrency setting allows.

**There are two claim paths, and they are not the same query.** A kind either uses
`serial_key` or it does not, and the difference is not a clause that can be switched off.

*Unserialized kinds (the distiller)* use the standard Postgres queue pattern: claim a
batch with `FOR UPDATE SKIP LOCKED`, work it, mark each job done or failed. Any worker may
take any job, so `SKIP LOCKED` is the whole mechanism.

```sql
UPDATE queue_job SET state = 'running', started_at = now(), attempt = attempt + 1
WHERE id IN (
    SELECT j.id FROM queue_job j
    WHERE j.kind = $1 AND j.state = 'pending' AND j.run_after <= now()
    ORDER BY j.priority DESC, j.run_after
    FOR UPDATE SKIP LOCKED
    LIMIT $2)
RETURNING ...;
```

*Serialized kinds (the assertion worker)* claim **one job at a time**, under an advisory
lock:

```sql
-- in the claim transaction, before the UPDATE:
SELECT pg_advisory_xact_lock(hashtext('claim:' || $1));

UPDATE queue_job SET state = 'running', started_at = now(), attempt = attempt + 1
WHERE state = 'pending' AND id = (
    SELECT j.id FROM queue_job j
    WHERE j.kind = $1 AND j.state = 'pending' AND j.run_after <= now()
      AND NOT EXISTS (
            SELECT 1 FROM queue_job r
            WHERE r.kind = j.kind AND r.state = 'running'
              AND r.serial_key = j.serial_key)
    ORDER BY j.priority DESC, j.run_after
    LIMIT 1)
RETURNING ...;
```

The advisory lock is what serializes claims here, so this path does not need
`FOR UPDATE SKIP LOCKED`; the repeated `state = 'pending'` on the outer `UPDATE` is a
cheap guard so the statement cannot claim a row that the lease reclaimer moved underneath
it, and a claim that returns no row is a normal empty poll.

Two separate things would each break the invariant, and each needs its own guard:

- **Across transactions**, two workers claiming concurrently would each read a snapshot in
  which the other's job is not yet `running`, and both would take the same key. The
  advisory lock on the kind makes the running-check and the update atomic with respect to
  other claims. It is held for one small `UPDATE`, not for the job, so jobs still run in
  parallel across different keys.
- **Within one statement**, a batch claim would break it even with the lock held. The
  `NOT EXISTS` is a correlated subquery evaluated against the statement's snapshot, in
  which *none* of the candidates is `running` yet, so a `LIMIT 5` over five pending jobs
  sharing one `serial_key` would pass all five and set all five to `running` in one
  `UPDATE`. `LIMIT 1` is what prevents this: one job per claim, so the next claim sees the
  previous one as `running` and skips its key.

`LIMIT 1` costs throughput, and it is affordable precisely here: the assertion worker is
low volume by design, and its jobs are model calls measured in seconds, so a claim
round-trip per job is noise. If a serialized kind ever needs batching, the shape that
works is a `LATERAL` picking one job per distinct eligible `serial_key`; that is a change
to make when something measures it, not now. Note that the obvious `SELECT DISTINCT ON
(serial_key) ... FOR UPDATE` is not an option — Postgres rejects `FOR UPDATE` with
`DISTINCT`.

**Wakeup is `LISTEN`/`NOTIFY` with a polling floor.** Workers listen on a channel per job
kind for low latency, and also poll on an interval (a few seconds) so that a missed
notification during a reconnect delays a job rather than stranding it. `NOTIFY` is fired
on transaction commit, so it cannot announce a job that then rolls back.

**Semantics are at-least-once.** A worker that dies mid-job leaves a `running` row whose
lease expires and is reclaimed. Both workers are idempotent, which is what makes this
acceptable: re-distilling an L0 event produces the same L1 doc, and the partial unique
index on `(kind, target_id)` for pending work, with the `ON CONFLICT DO NOTHING` enqueue
above, collapses a duplicate enqueue rather than doubling the model spend.

**Retries** are bounded, with exponential backoff and jitter, by setting `run_after`.
After the limit the job moves to `failed` and stays in the table. A failed job is a
visible row and a metric (ADR-0008), not a lost event; the L0 row it came from is still
there, so replaying is re-enqueueing.

**Scheduling is FIFO within a kind**, with an integer `priority` column for the cases
that will come later (a live directive that a human is waiting on should not queue behind
a backfill of three years of Slack).

Completed jobs are deleted after a retention window. Failed jobs are kept.

## Alternatives considered

- **[River](https://riverqueue.com).** The strongest alternative by some distance: a
  mature Postgres queue for Go, built on pgx (ADR-0004), with transactional enqueue,
  unique jobs, retries, periodic jobs and a UI. It would give most of the above for free.
  Rejected on the one requirement that is not negotiable: River's concurrency controls
  are per queue, so per-scope serialization would mean either a queue per scope, which is
  dynamic and unbounded, or taking an advisory lock inside the job and blocking a worker
  slot while another scope's work waits. Building the claim directly costs a table and two
  claim queries, and `serial_key` falls out of it. That is not free — the serialized claim
  path above is the fiddliest code in this decision, and it is the part most likely to be
  subtly wrong — but it is bounded, and it is code we can test directly against the
  invariant the design cares about. If Hearsay's queue needs grow toward what River
  offers, adopting it and solving serialization its way is a contained change, because
  callers use `internal/queue`, not SQL.
- **pgmq.** A Postgres extension providing SQS-like queues. Adds an extension to install
  alongside pgvector, and has the same per-key serialization gap.
- **Redis-backed (Asynq, Machinery).** Good queues. Both mean a second stateful service
  in every deployment, and enqueue stops being transactional with the L0 write, which
  reintroduces the gap this decision exists to close. The design wants one store.
- **NATS JetStream or Kafka.** Right answer at a volume Hearsay does not have. A
  single-organization event stream is thousands of events a day, not millions a minute.
- **Temporal.** Durable execution would suit a multi-step distillation pipeline well.
  Operationally far heavier than the rest of the system combined, for a self-hosted
  product where an operator is trying to run one compose file.
- **No queue: workers poll the L0 and L1 tables for unprocessed rows.** Genuinely
  tempting, and the state is already there (`l1` rows exist or they do not). Rejected
  because retries, backoff, failure visibility and per-scope serialization all have to be
  built anyway, and building them as columns on L0 mixes pipeline bookkeeping into an
  append-only event log that the design says is the record of what happened.

## Consequences

- One store still. Nothing to add to the compose file, nothing extra to back up, and a
  restore brings back the queue in the same state as the data.
- Queue load is database load. A distiller backfill hammering the queue table shares a
  Postgres with the API's read path. The claim queries are indexed for it
  (`(kind, state, run_after)` partial on pending, and `(kind, state, serial_key)` for the
  serialization check), plus the partial unique index on
  `(kind, target_id) WHERE state = 'pending'` that the enqueue relies on. If it ever
  becomes the bottleneck, the fix is a connection pool split before it is a different
  queue.
- Claims for serialized kinds are themselves serialized: one advisory lock per kind, one
  job per claim. Throughput for such a kind is bounded by claim round-trips rather than by
  worker count, which is fine for the assertion worker and would not be for a high-volume
  kind. Adding `serial_key` to a busy kind is therefore a decision to revisit this ADR,
  not a config change.
- The per-scope serialization invariant is testable and must be tested: concurrent workers
  against pending jobs sharing one `serial_key` must never show two running at once. That
  test is the reason for preferring code we own here over a library we would have to bend.
- `LISTEN`/`NOTIFY` needs a dedicated connection per listening worker, held outside the
  pool, and reconnect handling. The polling floor is what keeps a bug there from being an
  outage.
- Idempotency is now load-bearing rather than a nice property. The distiller and
  assertion worker must stay safe to run twice on the same input, and their tests have to
  assert it.
- Long-running jobs need lease renewal, or a slow model call will be reclaimed and run
  twice. The lease duration is config, and the worker heartbeats while working.
- Job payloads stay small: ids and a kind, never document text. The row to work on is
  fetched from its table, so a job never carries a stale copy of the thing it is about.
- Operators get queue depth, oldest pending age, and failure counts per kind as metrics
  (ADR-0008). Queue depth by kind is the first number to look at when the system feels
  slow.
