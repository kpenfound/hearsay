# 27. Distill jobs are debounced in the queue

- Status: accepted
- Date: 2026-10-15
- Supersedes: the clause of [ADR-0007](0007-postgres-backed-job-queue.md) that a duplicate enqueue of a pending target does nothing, for debounced requests only

## Context

A burst of L0 events on one artifact can otherwise cost several distillations
as workers claim jobs between pump reads. A distill job is idempotent and reads
current L0 state, rather than a snapshot in its payload. Coalescing those events
per document loses nothing and reduces model calls for every source.

The pump enqueues in the same transaction that saves its feed cursor. `watch`
relies on that invariant: it reports an event only once no unfinished distill
job targets the document, and a pending job counts as unfinished. Delaying
enqueue outside that transaction would leave a gap where `watch` could report
an event before it is distilled.

Operator deletion promises a rebuild without a debounce's delay
([ADR-0018](0018-operator-deletion-redacts-l0-in-place.md)). A GitHub command
waiting for its document must also get distillation promptly enough to complete
within the assert job's existing attempts.

## Decision

**Distill jobs are debounced per document in the Postgres queue.** The pump
continues enqueueing in its cursor-saving transaction, with `queue.Debounce`
when enabled. `HEARSAY_DISTILL_QUIET` defaults to `30s`; `0` disables it.
`HEARSAY_DISTILL_MAX_DELAY` defaults to `5m` and must be at least the quiet
period when debouncing is enabled. Both are Go durations, also available as
`--distill-quiet` and `--distill-max-delay` on `distiller` and `all`.

A new pending job becomes due at the database's `now() + Quiet`. A duplicate
debounced enqueue for a pending job that is not yet due updates only its
`run_after` to `LEAST(GREATEST(run_after, now() + Quiet), enqueued_at + Max)`.
The first `enqueued_at` never moves. The existing partial unique index on
`(kind, target_id) WHERE state = 'pending'` still resolves conflicts without
aborting the caller's transaction, and notifications are delivered on commit.
A running job permits a new pending job, since it may have read earlier state.
Invalid debounce bounds, or a debounce combined with `Delay`, are refused
before any SQL runs.

**A due job stays due.** No debounced enqueue postpones an already-due pending
job, whether it became due by quiet, the maximum delay, a non-debounced insert
or an expedite. The conflict guard compares `run_after` to Postgres's
`clock_timestamp()` when checking the row, including after waiting for a row
lock. Transaction-start `now()` or statement-start time could precede a
concurrent expedite's commit and incorrectly postpone that job.

`queue.Expedite` makes a pending job due now and notifies its workers.
Operator deletion enqueues without a debounce and expedites in the same
transaction. A waiting GitHub command attempt rolls back its command
transaction, then commits an expedite independently, leaving no command
effect or reply from that attempt. Commands with a readable L1 keep acting
immediately as before. Subsequent pump reads cannot postpone these due jobs.

Only distill requests use the debounce. Non-debounced enqueue retains
ADR-0007's `ON CONFLICT DO NOTHING` behavior exactly, and
[ADR-0011](0011-a-retry-superseded-by-a-pending-job-is-done.md)'s retry
supersession rule is unchanged. No worker wake timer is added: a job becoming
due is claimed at a following poll.

## Alternatives considered

- **Debounce in the pump before enqueueing.** Breaks the cursor/queue invariant
  that keeps `watch` from reporting undistilled events.
- **Keep extending the quiet period without a maximum.** A steady stream
  could prevent a document from ever being distilled.
- **Apply the update formula to due jobs too.** A pump read after an expedite
  would hold deletion and waiting GitHub commands behind the debounce again.
- **Batch several documents into one provider request.** Does not coalesce
  repeated events on one document and is a separate provider concern.

## Consequences

- A burst inside the quiet period costs one distill job and one model call per
  document. Rebuilds still call the model even if the resulting text is unchanged.
- `watch` keeps withholding events while their distill job is pending. The
  debounce adds up to the maximum delay before distillation becomes due,
  followed by worker poll and distillation time; a short long-poll may be empty.
- Maximum delay bounds when a job becomes due, not when distillation finishes.
  Queue load, worker availability and provider latency still affect completion.
- Queue timing and concurrent expedites are tested against Postgres, including
  an enqueue transaction or statement that began before the expedite committed.
