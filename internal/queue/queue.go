package queue

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is what [Enqueue] runs on: a pgx pool, or — the point of the
// interface — a transaction the caller is already in. A job row is written in
// the same transaction as the L0 or L1 write that causes it, so either both
// happen or neither does (ADR-0007).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Both of pgx's are one.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// jobColumns is a job row, in the order scanJobs reads it. Unqualified, so
// that it serves a plain SELECT and an UPDATE ... RETURNING alike. The kind is
// not among them: every statement here is already filtered to one kind, and
// the client knows whether that kind is serialized, which a row does not.
const jobColumns = `id, target_id, serial_key, priority, state, attempt,
	trace_context, enqueued_at, run_after, started_at, finished_at, last_error`

// enqueueSQL inserts the job, or does nothing because one is already pending
// for this target.
//
// ON CONFLICT ... DO NOTHING is not an optimisation here: the enqueue shares
// the caller's transaction, and a unique violation would abort the L0 or L1
// write that was the point of it (ADR-0007). The WHERE clause is what lets
// Postgres infer the partial index.
//
// The notification is fired from the same statement, which means it is
// delivered when this transaction commits and not at all if it rolls back —
// so no worker is ever woken for a job that does not exist.
const enqueueSQL = `
WITH inserted AS (
    INSERT INTO queue_job (kind, target_id, serial_key, priority, run_after, trace_context)
    VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5), $6)
    ON CONFLICT (kind, target_id) WHERE state = 'pending' DO NOTHING
    RETURNING id
)
SELECT inserted.id FROM inserted, LATERAL (SELECT pg_notify($7, '')) AS announced`

// Enqueue adds one job, inside whatever transaction the caller is in.
//
// It cannot abort that transaction: the request is validated before any SQL
// runs, and a job of the same kind already pending for the same target
// collapses into that one rather than raising a unique violation. Enqueueing
// again while a job is *running* is a new job, because the running one may
// already have read the state this enqueue is about.
func Enqueue(ctx context.Context, q Querier, req Request) (Enqueued, error) {
	if err := req.Validate(); err != nil {
		return Enqueued{}, err
	}
	trace, err := json.Marshal(traceOf(req.TraceContext))
	if err != nil {
		return Enqueued{}, fmt.Errorf("encoding the trace context of a %s job: %w", req.Kind.Name, err)
	}

	var id int64
	err = q.QueryRow(ctx, enqueueSQL,
		req.Kind.Name, req.TargetID, req.SerialKey, req.Priority,
		req.Delay.Seconds(), trace, req.Kind.Channel(),
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Enqueued{}, nil
	}
	if err != nil {
		return Enqueued{}, fmt.Errorf("enqueueing a %s job for %s: %w", req.Kind.Name, req.TargetID, err)
	}
	return Enqueued{ID: id, Stored: true}, nil
}

// traceOf is the carrier as it is stored: never null, so the column's default
// and what Go writes are the same thing.
func traceOf(carrier map[string]string) map[string]string {
	if carrier == nil {
		return map[string]string{}
	}
	return carrier
}

// Client is the consumer side of one job kind: claiming, reporting outcomes,
// reclaiming expired leases, retention and the counts an operator reads. It is
// safe for concurrent use, and [Worker] is the loop built on it.
//
// Everything it does is scoped to Config.Kind. Nothing outside this package
// runs SQL against the queue tables — the claim invariants stop being
// enforceable if it does (see the package README).
type Client struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New returns a client on an open pool. The caller owns the pool and closes
// it; the schema is the caller's to have checked (internal/db.Connect).
func New(pool *pgxpool.Pool, cfg Config) (*Client, error) {
	if pool == nil {
		return nil, errors.New("the queue needs a database pool")
	}
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Client{pool: pool, cfg: cfg}, nil
}

// Config is the configuration in force, with the defaults filled in.
func (c *Client) Config() Config { return c.cfg }

// claimSQL is the unserialized claim: a batch, taken with FOR UPDATE SKIP
// LOCKED so that two workers scanning the same backlog step over each other's
// rows rather than blocking on them (ADR-0007).
//
// The id is the last term of the ordering. Jobs enqueued in one transaction
// share run_after to the microsecond, and FIFO within a kind is only true if
// something breaks that tie.
const claimSQL = `
UPDATE queue_job SET state = 'running', started_at = now(), attempt = attempt + 1,
       lease_expires_at = now() + make_interval(secs => $3)
WHERE id IN (
    SELECT j.id FROM queue_job j
    WHERE j.kind = $1 AND j.state = 'pending' AND j.run_after <= now()
    ORDER BY j.priority DESC, j.run_after, j.id
    FOR UPDATE SKIP LOCKED
    LIMIT $2)
RETURNING ` + jobColumns

// claimSerialLockSQL takes the kind's claim lock for the rest of the
// transaction. It is a separate statement on purpose: a snapshot is taken when
// a statement begins, so a claim that acquired the lock in the same statement
// as its UPDATE would read the candidates from before the lock was granted —
// which is the snapshot in which the other worker's job is not running yet.
//
// That reasoning is READ COMMITTED's, and the claim requires it rather than
// merely expecting it — which is why claimSerialized asks for the level
// explicitly instead of inheriting default_transaction_isolation. Above READ
// COMMITTED the snapshot belongs to the transaction rather than to the
// statement, and it is registered by this statement, before the lock is
// granted: a worker that waited here would then run its UPDATE against a
// snapshot from before the holder committed, find no running job for the key,
// and claim a second one. Splitting the statements buys nothing there, and the
// advisory lock stops enforcing the invariant it exists for.
const claimSerialLockSQL = `SELECT pg_advisory_xact_lock(hashtext('claim:' || $1))`

// claimSerialSQL is the serialized claim: one job, whose serial key no running
// job of this kind holds.
//
// LIMIT 1 is load-bearing, not a throughput choice. NOT EXISTS is evaluated
// against this statement's snapshot, in which none of the candidates is
// running, so a batch would pass every pending job sharing one serial key and
// set them all running in one UPDATE — with the advisory lock held throughout.
// The repeated state = 'pending' on the outer UPDATE is the cheap guard
// against claiming a row the reclaimer moved underneath the subquery.
const claimSerialSQL = `
UPDATE queue_job SET state = 'running', started_at = now(), attempt = attempt + 1,
       lease_expires_at = now() + make_interval(secs => $2)
WHERE state = 'pending' AND id = (
    SELECT j.id FROM queue_job j
    WHERE j.kind = $1 AND j.state = 'pending' AND j.run_after <= now()
      AND NOT EXISTS (
            SELECT 1 FROM queue_job r
            WHERE r.kind = j.kind AND r.state = 'running'
              AND r.serial_key = j.serial_key)
    ORDER BY j.priority DESC, j.run_after, j.id
    LIMIT 1)
RETURNING ` + jobColumns

// Claim takes up to Config.Concurrency jobs and marks them running, with a
// lease of Config.Lease on each. An empty result is a normal empty poll.
//
// Which of the two claim paths runs is decided by the kind, and they are not
// interchangeable (ADR-0007): an unserialized kind claims a batch with FOR
// UPDATE SKIP LOCKED, and a serialized one claims a single job under an
// advisory lock on the kind, so that two claims cannot both decide that a
// serial key is free.
func (c *Client) Claim(ctx context.Context) ([]Job, error) {
	if c.cfg.Kind.Serialized {
		return c.claimSerialized(ctx)
	}
	rows, err := c.pool.Query(ctx, claimSQL, c.cfg.Kind.Name, c.cfg.Concurrency, c.cfg.Lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claiming %s jobs: %w", c.cfg.Kind.Name, err)
	}
	jobs, err := c.scanJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("claiming %s jobs: %w", c.cfg.Kind.Name, err)
	}
	return inWorkOrder(jobs), nil
}

// claimSerialized runs the advisory lock and the claim in one transaction: the
// lock is held until it commits, which is what makes the running-check and the
// update atomic against other claims. It is held for one small UPDATE and not
// for the job, so different serial keys still run in parallel.
//
// The isolation level is named rather than inherited. Nothing else pins it —
// db.Open passes the URL to pgx as it is, so the level is the server's
// default_transaction_isolation, which an operator sets in postgresql.conf, on
// the database, on the role, or in the connection URL — and the serialization
// invariant only holds under READ COMMITTED. See claimSerialLockSQL.
func (c *Client) claimSerialized(ctx context.Context) ([]Job, error) {
	fail := func(err error) error {
		return fmt.Errorf("claiming a %s job: %w", c.cfg.Kind.Name, err)
	}
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fail(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.Exec(ctx, claimSerialLockSQL, c.cfg.Kind.Name); err != nil {
		return nil, fail(err)
	}
	rows, err := tx.Query(ctx, claimSerialSQL, c.cfg.Kind.Name, c.cfg.Lease.Seconds())
	if err != nil {
		return nil, fail(err)
	}
	jobs, err := c.scanJobs(rows)
	if err != nil {
		return nil, fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fail(err)
	}
	return jobs, nil
}

// completeSQL finishes a job, and matches on the run rather than on the job:
// a worker whose lease expired while it was working has had its job reclaimed
// and possibly re-run, and must not mark that other run done.
const completeSQL = `
UPDATE queue_job SET state = 'done', finished_at = now(), lease_expires_at = NULL,
       last_error = NULL
WHERE id = $1 AND attempt = $2 AND state = 'running'
RETURNING id`

// Complete marks a job done. It reports false when the job was no longer this
// run's to finish — the lease expired and something reclaimed it — in which
// case nothing was written.
func (c *Client) Complete(ctx context.Context, job Job) (bool, error) {
	var id int64
	err := c.pool.QueryRow(ctx, completeSQL, job.ID, job.Attempt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("completing %s: %w", job, err)
	}
	return true, nil
}

// failSQL either schedules a retry or gives up, in one statement so that the
// decision and the write cannot disagree. $3 is whether the attempts are
// spent: a job that has them left goes back to pending with run_after moved,
// and one that does not moves to failed and stays there.
const failSQL = `
UPDATE queue_job SET
    state = CASE WHEN $3 THEN 'failed' ELSE 'pending' END,
    finished_at = CASE WHEN $3 THEN now() ELSE NULL END,
    started_at = CASE WHEN $3 THEN started_at ELSE NULL END,
    run_after = CASE WHEN $3 THEN run_after ELSE now() + make_interval(secs => $4) END,
    lease_expires_at = NULL,
    last_error = $5
WHERE id = $1 AND attempt = $2 AND state = 'running'
RETURNING state, run_after`

// Fail records why an attempt did not work and decides what happens next: a
// retry, at a backed-off and jittered run_after, or the failed state, which a
// job stays in and is queryable from (ADR-0007 — a failed job is a visible row
// and a metric, not a lost event).
//
// It returns the state the job is now in and, for a retry, when it runs again.
// The state is empty when the job was no longer this run's to report on, in
// which case nothing was written.
func (c *Client) Fail(ctx context.Context, job Job, cause error) (State, Retry, error) {
	spent := job.Attempt >= c.cfg.MaxAttempts
	wait := time.Duration(0)
	if !spent {
		wait = c.cfg.RetryDelay(job.Attempt)
	}

	var state State
	var runAfter time.Time
	err := c.pool.QueryRow(ctx, failSQL, job.ID, job.Attempt, spent, wait.Seconds(), errorText(cause)).
		Scan(&state, &runAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", Retry{}, nil
	}
	if err != nil {
		return "", Retry{}, fmt.Errorf("failing %s: %w", job, err)
	}
	if spent {
		// The job is not running again, and a run_after left where the last
		// attempt put it is not a schedule.
		return state, Retry{}, nil
	}
	return state, Retry{In: wait, At: runAfter.UTC()}, nil
}

// Retry is when a failed attempt runs again. It is zero on a job that has run
// out of attempts.
type Retry struct {
	// In is the delay that was applied.
	In time.Duration
	// At is when the job becomes claimable again, as Postgres computed it.
	At time.Time
}

// heartbeatSQL extends the lease on one run.
const heartbeatSQL = `
UPDATE queue_job SET lease_expires_at = now() + make_interval(secs => $3)
WHERE id = $1 AND attempt = $2 AND state = 'running'
RETURNING id`

// Heartbeat renews the lease on a job that is still being worked on, so that a
// slow model call is not reclaimed mid-flight. It reports false when the job
// is no longer this run's — the lease expired and it was reclaimed — which is
// the worker's signal to stop working on it.
func (c *Client) Heartbeat(ctx context.Context, job Job) (bool, error) {
	var id int64
	err := c.pool.QueryRow(ctx, heartbeatSQL, job.ID, job.Attempt, c.cfg.Lease.Seconds()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("renewing the lease on %s: %w", job, err)
	}
	return true, nil
}

// reclaimSQL takes back the jobs of workers that stopped heartbeating. There
// is no backoff on a reclaimed job: a worker that died is not a job that is
// failing, so it runs as soon as something claims it. A job that has used its
// attempts up fails here rather than being handed round a crashing worker for
// ever.
const reclaimSQL = `
UPDATE queue_job SET
    state = CASE WHEN attempt >= $2 THEN 'failed' ELSE 'pending' END,
    finished_at = CASE WHEN attempt >= $2 THEN now() ELSE NULL END,
    started_at = CASE WHEN attempt >= $2 THEN started_at ELSE NULL END,
    lease_expires_at = NULL,
    last_error = $3
WHERE kind = $1 AND state = 'running' AND lease_expires_at < now()
RETURNING state`

// Reclaimed is what one sweep of the expired leases did.
type Reclaimed struct {
	// Pending is how many jobs went back to the queue.
	Pending int
	// Failed is how many had no attempts left.
	Failed int
}

// Total is how many jobs the sweep touched.
func (r Reclaimed) Total() int { return r.Pending + r.Failed }

// Reclaim takes back every job of this kind whose lease has expired. It is
// what makes delivery at-least-once: a worker that dies mid-job leaves a
// running row, and this is what turns it back into work. A worker calls it on
// its maintenance interval; nothing else has to.
func (c *Client) Reclaim(ctx context.Context) (Reclaimed, error) {
	rows, err := c.pool.Query(ctx, reclaimSQL, c.cfg.Kind.Name, c.cfg.MaxAttempts,
		fmt.Sprintf("the lease expired after %s without a heartbeat", c.cfg.Lease))
	if err != nil {
		return Reclaimed{}, fmt.Errorf("reclaiming expired %s jobs: %w", c.cfg.Kind.Name, err)
	}
	defer rows.Close()

	var out Reclaimed
	for rows.Next() {
		var state State
		if err := rows.Scan(&state); err != nil {
			return Reclaimed{}, fmt.Errorf("reclaiming expired %s jobs: %w", c.cfg.Kind.Name, err)
		}
		if state == StateFailed {
			out.Failed++
		} else {
			out.Pending++
		}
	}
	if err := rows.Err(); err != nil {
		return Reclaimed{}, fmt.Errorf("reclaiming expired %s jobs: %w", c.cfg.Kind.Name, err)
	}
	return out, nil
}

// purgeSQL deletes done jobs past the retention window. Failed jobs are not
// mentioned: they are kept.
const purgeSQL = `
DELETE FROM queue_job
WHERE kind = $1 AND state = 'done' AND finished_at < now() - make_interval(secs => $2)`

// Purge deletes this kind's done jobs older than Config.Retention and reports
// how many went. Failed jobs are kept however old they are — they are the
// record of what did not happen.
func (c *Client) Purge(ctx context.Context) (int64, error) {
	tag, err := c.pool.Exec(ctx, purgeSQL, c.cfg.Kind.Name, c.cfg.Retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("purging done %s jobs: %w", c.cfg.Kind.Name, err)
	}
	return tag.RowsAffected(), nil
}

// listSQL reads one state's jobs, oldest first.
const listSQL = `
SELECT ` + jobColumns + `
  FROM queue_job
 WHERE kind = $1 AND state = $2
 ORDER BY id
 LIMIT $3`

// Limits on how much one listing returns.
const (
	// DefaultLimit is what a listing with no limit returns.
	DefaultLimit = 100
	// MaxLimit is the most any listing returns.
	MaxLimit = 1000
)

// List returns this kind's jobs in one state, oldest first. It is how a failed
// job stays queryable, and the only read of the queue tables anything outside
// this package gets.
func (c *Client) List(ctx context.Context, state State, limit int) ([]Job, error) {
	if !state.Valid() {
		return nil, fmt.Errorf("%w: %q is not a job state", ErrInvalidJob, state)
	}
	rows, err := c.pool.Query(ctx, listSQL, c.cfg.Kind.Name, string(state), Limit(limit))
	if err != nil {
		return nil, fmt.Errorf("listing %s %s jobs: %w", state, c.cfg.Kind.Name, err)
	}
	jobs, err := c.scanJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("listing %s %s jobs: %w", state, c.cfg.Kind.Name, err)
	}
	return jobs, nil
}

// Limit is how many jobs a listing with this limit actually returns: the
// default when none was asked for, the cap when too many were.
func Limit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultLimit
	case limit > MaxLimit:
		return MaxLimit
	default:
		return limit
	}
}

// statsSQL counts one kind by state in a single pass, and takes the age of the
// oldest job that is ready to run — which is queue latency, as against a
// backlog of jobs that are simply not due yet.
const statsSQL = `
SELECT count(*) FILTER (WHERE state = 'pending'),
       count(*) FILTER (WHERE state = 'running'),
       count(*) FILTER (WHERE state = 'done'),
       count(*) FILTER (WHERE state = 'failed'),
       coalesce(extract(epoch FROM now() -
           min(run_after) FILTER (WHERE state = 'pending' AND run_after <= now())), 0)::double precision
  FROM queue_job
 WHERE kind = $1`

// Stats is a kind's queue depth and how far behind it is: the numbers ADR-0008
// asks an operator to be given, and the first ones to look at when the system
// feels slow.
type Stats struct {
	Kind    string
	Pending int64
	Running int64
	Done    int64
	Failed  int64
	// OldestPending is how long the oldest job that is ready to run has been
	// waiting. Zero when nothing is due.
	OldestPending time.Duration
}

// Stats reads the kind's counts.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	s := Stats{Kind: c.cfg.Kind.Name}
	var oldest float64
	err := c.pool.QueryRow(ctx, statsSQL, c.cfg.Kind.Name).
		Scan(&s.Pending, &s.Running, &s.Done, &s.Failed, &oldest)
	if err != nil {
		return Stats{}, fmt.Errorf("reading %s queue stats: %w", c.cfg.Kind.Name, err)
	}
	s.OldestPending = time.Duration(oldest * float64(time.Second))
	return s, nil
}

// inWorkOrder sorts a claimed batch the way the claim chose it: priority
// first, then oldest first. RETURNING hands rows back in whatever order the
// UPDATE walked them, which is not the ORDER BY that decided *which* rows it
// took, so a caller that works a batch in order would otherwise be working it
// in an arbitrary one.
func inWorkOrder(jobs []Job) []Job {
	slices.SortFunc(jobs, func(a, b Job) int {
		if d := cmp.Compare(b.Priority, a.Priority); d != 0 {
			return d
		}
		if d := a.RunAfter.Compare(b.RunAfter); d != 0 {
			return d
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return jobs
}

// scanJobs reads a result set of job rows and closes it.
func (c *Client) scanJobs(rows pgx.Rows) ([]Job, error) {
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		var (
			job        Job
			trace      []byte
			startedAt  *time.Time
			finishedAt *time.Time
			lastError  *string
		)
		if err := rows.Scan(&job.ID, &job.TargetID, &job.SerialKey, &job.Priority, &job.State,
			&job.Attempt, &trace, &job.EnqueuedAt, &job.RunAfter,
			&startedAt, &finishedAt, &lastError); err != nil {
			return nil, err
		}
		job.Kind = c.cfg.Kind
		// Postgres hands a timestamptz back in the session's time zone, which
		// is the server's; reading them all in UTC is what keeps two jobs from
		// looking different when they are not.
		job.EnqueuedAt = job.EnqueuedAt.UTC()
		job.RunAfter = job.RunAfter.UTC()
		if startedAt != nil {
			job.StartedAt = startedAt.UTC()
		}
		if finishedAt != nil {
			job.FinishedAt = finishedAt.UTC()
		}
		if lastError != nil {
			job.LastError = *lastError
		}
		if err := json.Unmarshal(trace, &job.TraceContext); err != nil {
			return nil, fmt.Errorf("decoding the trace context of job %d: %w", job.ID, err)
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}
