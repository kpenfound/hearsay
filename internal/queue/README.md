# internal/queue

The job queue. One Postgres, no broker (ADR-0007).

**Belongs here:** enqueue (transactional, deduplicating on `(kind, target_id)`
while a job is pending), the two claim paths, retries with backoff, the dead
letter path, `LISTEN`/`NOTIFY` plus the polling floor, and the worker loop.

**Does not belong here:** job handlers. What a `distill` or `assert` job does
belongs to the service that owns it. And no SQL against the queue tables from
outside this package — callers use the API, or the claim invariants stop being
enforceable.

The three things ADR-0007 asks an implementer not to simplify away:
transactional enqueue, two claim paths (batch `FOR UPDATE SKIP LOCKED` for
unserialized kinds, `LIMIT 1` under an advisory lock for serialized ones), and
the dedupe on enqueue. The reasons are in the ADR.

## Using it

A producer enqueues in the transaction that causes the job, so a job cannot
exist for a write that rolled back:

```go
kind := queue.Kind{Name: "distill"}                          // declared once
_, err := queue.Enqueue(ctx, tx, queue.Request{Kind: kind, TargetID: eventID})
```

A consumer runs a worker for one kind:

```go
worker, err := queue.NewWorker(pool, queue.Config{Kind: kind, Concurrency: 8}, handle)
err = worker.Run(ctx)   // returns nil when ctx is cancelled
```

`queue.Client` is what the worker is built on — `Claim`, `Complete`, `Fail`,
`Heartbeat`, `Reclaim`, `Purge`, `List` and `Stats` — and is also how an
operator reads a kind's depth or its failures. Every `Config` field may be left
zero; the defaults are the ones the constants document.

## Things to know before changing it

- **A `Kind` is shared by both ends.** Whether a kind is serialized is a
  property of the kind, not of a row: the table cannot tell, so the service
  that enqueues the work and the worker that runs it must use the same `Kind`
  value. `Request.Validate` refuses a serialized kind with no serial key, and
  an unserialized one with a key, which is what stops the common mistake.
- **`serial_key` is `''` and never null.** The serialization check is
  `r.serial_key = j.serial_key`, and under null that comparison is unknown —
  a key-less job on a serialized kind would pass it against every running job
  and escape serialization on the one path that exists to enforce it.
- **A batch claim under the advisory lock still breaks the invariant.**
  `LIMIT 1` on the serialized path is correctness, not throughput; ADR-0007
  works through why.
- **The invariant is tested, not asserted.** The serialized claim's test runs
  concurrent workers over jobs sharing a key and fails if two are ever running
  at once. Keep it that way — it is the reason ADR-0007 chose code we own over
  a library.

See [ADR-0007](../../docs/adr/0007-postgres-backed-job-queue.md).
