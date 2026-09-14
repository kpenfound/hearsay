# 11. A retry superseded by a pending job for its target is done, not requeued

- Status: accepted
- Date: 2026-09-14
- Supersedes: the at-least-once clause of [ADR-0007](0007-postgres-backed-job-queue.md), for a retry or reclaim whose target already has a pending job

## Context

ADR-0007 dedupes a kind's work on a partial unique index,
`queue_job (kind, target_id) WHERE state = 'pending'`, and gives delivery as:

> **Semantics are at-least-once.** A worker that dies mid-job leaves a `running` row whose
> lease expires and is reclaimed.

A running job does not occupy that index, on purpose: an event that arrives while
a job runs may postdate what the job read, so it is new work. A target can
therefore have one job running and one pending at the same time.

Two statements move a job back to `pending` — a failed attempt's retry
(`failSQL`) and the lease reclaimer (`reclaimSQL`) — and both then violate the
index. The retry's error was only logged, so the job stayed `running` until its
lease expired. The reclaimer is one `UPDATE` over every expired job of a kind, so
one colliding row aborted the sweep for the whole kind for as long as the
collision lasted. A provider outage, with jobs failing while events keep
arriving, is exactly the state that produces it (issue #65).

Two running jobs for one target are possible too — the second was enqueued while
the first ran, then claimed — and if both leases expire, requeueing both in one
statement collides with itself.

## Decision

A job that would go back to `pending` while its target already has a pending job
is **superseded**: it moves to `done`, with `finished_at` set, and its
`last_error` is its own cause prefixed with
`superseded by a pending job for the same target; `. This applies to every
target-deduped kind; nothing is special-cased by kind name.

- **Why done.** The pending job does the same work against a newer read of the
  target, which is what the dedupe already assumes of a collapsed enqueue. The
  retry has nothing left to contribute, and a `done` row is purged after the
  retention window like any other finished work. A `done` row otherwise never
  carries a `last_error`, so the prefix is what tells an operator reading one
  that it was superseded rather than completed.
- **A job with no attempts left still fails.** Failing does not touch the index,
  and a `failed` row is the record that a target used its attempts up, which an
  operator wants whether or not newer work is queued behind it.
- **Among expired running jobs for one target, the newest (by id) is requeued**
  and the older ones are superseded — unless the newer one is itself failing for
  lack of attempts, or its lease has not expired, in which case it is not going
  back to pending and the older one is requeued. The newest was enqueued after
  the older ones were claimed, so its read is the newest.
- **Reclaim decides per row, then writes the kind in one statement.** So one
  target's collision cannot stop every other expired job of the kind from being
  reclaimed.
- **A collision the statement could not see is retried.** The decision reads the
  statement's snapshot, so a pending job committed after it began is invisible
  and the `UPDATE` still raises 23505 on the index. `Fail` and `Reclaim` run the
  statement again (a few times, bounded) on that specific violation; the next
  snapshot sees the pending job and supersedes.
- `Client.Fail` returns `StateDone` for a superseded retry, and
  `Reclaimed.Superseded` counts the reclaimer's.

Delivery is still at-least-once *per target*: the work a superseded job stood
for is done by the pending job. It is no longer at-least-once *per job row*.

## Alternatives considered

- **Requeue by merging into the pending job** — delete the retry and move its
  `attempt` or `last_error` onto the pending row. Keeps one row per unit of work,
  but writes to a row another worker may be claiming, and blurs what the pending
  job's attempt count means. Rejected: supersession leaves each row describing
  its own run.
- **Supersede as `failed` with an explanation.** Visible, but failed rows are
  kept for ever and counted as failures in the metrics ADR-0008 asks for, so a
  provider outage with traffic would inflate the failure count with work that
  was not lost. Rejected.
- **Delete the superseded row.** The cheapest, and it loses the record of why an
  attempt ended, which is the one thing `last_error` is for.
- **Reclaim row by row.** Isolates each row's error without the per-row decision,
  but trades one statement for a round trip per expired job, and still needs a
  rule for what the colliding row becomes. Rejected: the decision is needed
  either way, and with it the single statement is safe.
- **An advisory lock per target around enqueue, retry and reclaim.** Would close
  the snapshot race without a retry, but puts a lock in every caller's
  transaction on the enqueue path, which ADR-0007 keeps free of anything that
  can abort or block the L0 or L1 write. Rejected.
- **Edit ADR-0007's at-least-once paragraph.** Forbidden by ADR-0001.

## Consequences

- A retry or reclaim can no longer violate the index that dedupes it, and one
  target cannot wedge a kind's reclaim sweep.
- The pending job starts with its own attempt budget. A target that fails every
  time while events keep arriving is retried per event rather than once; its
  failures show as `done` rows marked superseded and, for the job that finally
  runs out of attempts, a `failed` row.
- `Stats.Done` counts superseded jobs with completed ones. The two are told apart
  by `last_error`, not by a state; a state of their own would be a migration and
  a change to the schema's CHECK, which nothing needs yet.
- Handlers still have to be idempotent: a worker that dies mid-job is still
  reclaimed and run again when nothing is pending for its target.
