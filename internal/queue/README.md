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

See [ADR-0007](../../docs/adr/0007-postgres-backed-job-queue.md).
