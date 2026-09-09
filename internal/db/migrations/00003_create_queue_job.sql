-- +goose Up

-- The job queue (ADR-0007). One row per job, enqueued in the same transaction
-- as the write that caused it, claimed by a worker, and either done, failed, or
-- waiting for a retry. Only internal/queue reads or writes this table.
CREATE TABLE queue_job (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- kind is the job kind (`distill`, `assert`), and it is also the unit of
    -- everything else here: a worker claims one kind, listens on one channel
    -- per kind, and a kind is either serialized or it is not.
    kind text NOT NULL,

    -- target_id is the row the job is about — an L0 event id, an L1 doc id.
    -- Job payloads stay small: the worker fetches the row, so a job never
    -- carries a stale copy of the thing it is about.
    target_id text NOT NULL,

    -- serial_key is the scope two jobs must not run in at the same time; '' is
    -- "no key", for kinds that run as wide as their worker allows.
    --
    -- '' rather than NULL because the serialization check is the equality
    -- `r.serial_key = j.serial_key`: under NULL that comparison is unknown, so
    -- a key-less job on a serialized kind would pass the check against every
    -- running job and escape serialization on the one path that exists to
    -- enforce it. With '' the same job serializes with the other key-less jobs
    -- of its kind, which is wrong-but-safe rather than silently wrong.
    serial_key text NOT NULL DEFAULT '',

    -- Higher runs first, within a kind. For the live directive a human is
    -- waiting on, which should not queue behind a three-year backfill.
    priority integer NOT NULL DEFAULT 0,

    state text NOT NULL DEFAULT 'pending',

    -- attempt counts runs, not retries: it is incremented by the claim, so a
    -- running job's attempt is the number of this run and 1 is the first.
    -- Together with id it identifies one run, which is what lets a worker whose
    -- lease expired be refused when it reports an outcome late.
    attempt integer NOT NULL DEFAULT 0,

    -- The propagation carrier of the trace that enqueued the job, so that the
    -- work a worker does later joins the trace that caused it (ADR-0008).
    trace_context jsonb NOT NULL DEFAULT '{}'::jsonb,

    enqueued_at timestamptz NOT NULL DEFAULT now(),

    -- run_after is both the delay and the backoff: a retry is scheduled by
    -- moving it forward, and a claim never takes a job before it.
    run_after timestamptz NOT NULL DEFAULT now(),

    -- started_at is when the current run began, and lease_expires_at when the
    -- queue stops believing the worker is alive. The worker heartbeats while it
    -- works; a lease that expires is reclaimed, which is what makes delivery
    -- at-least-once rather than at-most-once.
    started_at timestamptz,
    lease_expires_at timestamptz,

    -- Terminal bookkeeping. last_error is kept on a retry too, so a job that is
    -- waiting to run again says why it is on its third attempt.
    finished_at timestamptz,
    last_error text,

    CONSTRAINT queue_job_state_is_known CHECK (
        state IN ('pending', 'running', 'done', 'failed')
    ),
    CONSTRAINT queue_job_attempt_is_not_negative CHECK (attempt >= 0),
    -- A lease belongs to a run. Requiring it on a running row and forbidding it
    -- on any other is what keeps the reclaimer's `lease_expires_at < now()`
    -- from ever matching a job nobody is running.
    CONSTRAINT queue_job_a_lease_is_a_running_job CHECK (
        (state = 'running') = (lease_expires_at IS NOT NULL)
    ),
    -- A pending job is not part-way through a run: a reclaimed or retried job
    -- clears started_at along with its lease.
    CONSTRAINT queue_job_pending_is_not_started CHECK (
        state <> 'pending' OR started_at IS NULL
    ),
    CONSTRAINT queue_job_finished_is_terminal CHECK (
        (state IN ('done', 'failed')) = (finished_at IS NOT NULL)
    )
);

-- Job identity while there is work outstanding, and the index the enqueue's
-- ON CONFLICT infers. Partial, because it is only a duplicate enqueue while the
-- first job is still pending: once a job is running, an event that arrives for
-- the same target is new work rather than the same work.
CREATE UNIQUE INDEX queue_job_pending_target_idx
    ON queue_job (kind, target_id) WHERE state = 'pending';

-- The claim's candidate scan: one kind's pending jobs, in run_after order.
-- Partial on pending, which is the only state a claim considers, and which
-- keeps the index the size of the backlog rather than of the history.
CREATE INDEX queue_job_claim_idx
    ON queue_job (kind, state, run_after) WHERE state = 'pending';

-- The serialized claim's NOT EXISTS, and the reclaimer's scan of expired
-- leases: both are a kind's running rows. Not partial, so that a count by state
-- for one kind — queue depth, the first number an operator looks at — is served
-- by it too.
CREATE INDEX queue_job_serial_idx ON queue_job (kind, state, serial_key);

-- +goose Down

-- Destroys every job, including the failed ones kept for an operator to look
-- at. `hearsay migrate down` refuses without --i-know (ADR-0006).
DROP TABLE queue_job;
