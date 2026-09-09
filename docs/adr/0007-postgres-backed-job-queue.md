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

**Claim is `FOR UPDATE SKIP LOCKED`**, the standard Postgres queue pattern: a worker
claims a batch, works it, and marks it done or failed. Roughly:

```sql
UPDATE queue_job SET state = 'running', started_at = now(), attempt = attempt + 1
WHERE id IN (
    SELECT j.id FROM queue_job j
    WHERE j.kind = $1 AND j.state = 'pending' AND j.run_after <= now()
      AND (j.serial_key IS NULL OR NOT EXISTS (
            SELECT 1 FROM queue_job r
            WHERE r.kind = j.kind AND r.state = 'running' AND r.serial_key = j.serial_key))
    ORDER BY j.priority DESC, j.run_after
    FOR UPDATE SKIP LOCKED
    LIMIT $2)
RETURNING ...;
```

**`serial_key` is the reason this is hand-written.** A job with a `serial_key` never runs
while another job with the same key is running. The assertion worker sets
`serial_key = scope`, which is exactly the serialization the design asks for, while
leaving different scopes fully parallel. The distiller leaves it null and runs as wide as
its concurrency setting allows.

**Wakeup is `LISTEN`/`NOTIFY` with a polling floor.** Workers listen on a channel per job
kind for low latency, and also poll on an interval (a few seconds) so that a missed
notification during a reconnect delays a job rather than stranding it. `NOTIFY` is fired
on transaction commit, so it cannot announce a job that then rolls back.

**Semantics are at-least-once.** A worker that dies mid-job leaves a `running` row whose
lease expires and is reclaimed. Both workers are idempotent, which is what makes this
acceptable: re-distilling an L0 event produces the same L1 doc, and job identity is a
unique key on `(kind, target_id)` for pending work so a duplicate enqueue collapses
rather than doubling the model spend.

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
  slot while another scope's work waits. Building the claim query directly costs one SQL
  statement and a table, and `serial_key` falls out of it. If Hearsay's queue needs grow
  toward what River offers, adopting it and solving serialization its way is a
  contained change, because callers use `internal/queue`, not SQL.
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
  Postgres with the API's read path. The claim query is indexed for it
  (`(kind, state, run_after)` partial on pending, and `(kind, state, serial_key)` for the
  serialization check), and if it ever becomes the bottleneck, the fix is a connection
  pool split before it is a different queue.
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
