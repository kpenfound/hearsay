package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// HoldPoll is how often [Client.Hold] looks again for the jobs it waits
// behind. They are model calls measured in seconds, so a quarter of a second
// is noise to them and short to a person waiting on a command.
const HoldPoll = 250 * time.Millisecond

// runningSQL is the ids of the jobs already running under a serial key: the
// ones a hold has to wait for. It runs under the kind's claim lock
// (claimSerialLockSQL), in a statement of its own after the lock, for the
// reason claimSerialized gives: so what it reads includes every claim
// committed before the lock was granted.
const runningSQL = `
SELECT coalesce(array_agg(id), '{}') FROM queue_job
WHERE kind = $1 AND state = 'running' AND serial_key = $2`

// holdSQL writes the caller's job as already running, under the same lock.
//
// The row goes in running whether or not the key is busy. That is what keeps
// a hold from starving behind a backlog: from the moment it commits, no claim
// takes the key, so the caller waits for the jobs already running and for
// nothing enqueued after them.
const holdSQL = `
INSERT INTO queue_job (kind, target_id, serial_key, state, attempt, started_at, lease_expires_at)
VALUES ($1, $3, $2, 'running', 1, now(), now() + make_interval(secs => $4))
RETURNING ` + jobColumns + `
`

// aheadSQL counts the jobs a hold waits for that are still running. A job
// that stops running cannot run again under the key while the hold stands: a
// claim skips a key with a running job, and the hold is one. A job whose lease
// has expired is not waited for: the queue has stopped believing its worker is
// alive, and a hold should not wait on a reclaimer when the kind's worker is
// down.
const aheadSQL = `SELECT count(*) FROM queue_job WHERE id = ANY($1) AND state = 'running' AND lease_expires_at > now()`

// Hold takes a serial key of a serialized kind for the caller rather than for
// a worker: it writes a job for target that is already running, then waits for
// the jobs that were running under the key to finish. When it returns, nothing
// else of the kind runs under the key until the caller completes the job
// ([Client.CompleteIn] in the transaction that does the work, or
// [Client.Complete] to let the key go without it). Other keys are not held.
//
// It is for work a person is waiting on that must be serialized with a kind's
// jobs and cannot wait for a worker to get round to it. The lease is the
// kind's, and Hold renews it while it waits; the caller does its work within
// one lease. A hold whose caller dies is reclaimed like any job, and the
// kind's worker then runs a job for target, which it must treat as done: the
// work either committed with the job's completion or did not happen.
//
// Two holds on one key wait for each other in the order they were taken.
// Cancelling ctx lets the key go and returns ctx's error.
func (c *Client) Hold(ctx context.Context, serialKey, target string) (Job, error) {
	req := Request{Kind: c.cfg.Kind, TargetID: target, SerialKey: serialKey}
	if err := req.Validate(); err != nil {
		return Job{}, err
	}
	if !c.cfg.Kind.Serialized {
		return Job{}, fmt.Errorf("%w: %s is not a serialized kind, so it has no key to hold", ErrInvalidJob, c.cfg.Kind.Name)
	}
	job, ahead, err := c.hold(ctx, req)
	if err != nil {
		return Job{}, err
	}
	if err := c.waitFor(ctx, job, ahead); err != nil {
		// The key goes back whatever the context says: the caller has not
		// started, and a hold left running blocks the key for a lease.
		if _, done := c.Complete(context.WithoutCancel(ctx), job); done != nil {
			return Job{}, errors.Join(err, done)
		}
		return Job{}, err
	}
	return job, nil
}

func (c *Client) hold(ctx context.Context, req Request) (Job, []int64, error) {
	fail := func(err error) error {
		return fmt.Errorf("holding %s key %q: %w", c.cfg.Kind.Name, req.SerialKey, err)
	}
	// READ COMMITTED by name, as claimSerialized asks for it and for the same
	// reason: the running jobs are read by a statement that starts after the
	// lock is granted.
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Job{}, nil, fail(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a rollback after commit is a no-op

	if _, err := tx.Exec(ctx, claimSerialLockSQL, c.cfg.Kind.Name); err != nil {
		return Job{}, nil, fail(err)
	}
	var ahead []int64
	if err := tx.QueryRow(ctx, runningSQL, c.cfg.Kind.Name, req.SerialKey).Scan(&ahead); err != nil {
		return Job{}, nil, fail(err)
	}
	rows, err := tx.Query(ctx, holdSQL, c.cfg.Kind.Name, req.SerialKey, req.TargetID, c.cfg.Lease.Seconds())
	if err != nil {
		return Job{}, nil, fail(err)
	}
	jobs, err := c.scanJobs(rows)
	if err != nil {
		return Job{}, nil, fail(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, nil, fail(err)
	}
	return jobs[0], ahead, nil
}

// waitFor returns once none of the jobs ahead is running, renewing the hold's
// lease as a worker renews a claim's.
func (c *Client) waitFor(ctx context.Context, job Job, ahead []int64) error {
	if len(ahead) == 0 {
		return nil
	}
	renewed := time.Now()
	for {
		var running int
		if err := c.pool.QueryRow(ctx, aheadSQL, ahead).Scan(&running); err != nil {
			return fmt.Errorf("waiting to hold %s key %q: %w", c.cfg.Kind.Name, job.SerialKey, err)
		}
		if running == 0 {
			return nil
		}
		if time.Since(renewed) >= c.cfg.HeartbeatInterval() {
			held, err := c.Heartbeat(ctx, job)
			if err != nil {
				return err
			}
			if !held {
				return fmt.Errorf("holding %s key %q: the hold's lease expired while it waited", c.cfg.Kind.Name, job.SerialKey)
			}
			renewed = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(HoldPoll):
		}
	}
}

// CompleteIn is [Client.Complete] in the caller's transaction, so that the
// work a hold was taken for and the end of the hold commit together. It
// reports false when the job is no longer this run's — a hold whose lease
// expired and was reclaimed, so the key may already be another job's — and
// the caller must then roll its work back.
func (c *Client) CompleteIn(ctx context.Context, q Querier, job Job) (bool, error) {
	var id int64
	err := q.QueryRow(ctx, completeSQL, job.ID, job.Attempt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("completing %s: %w", job, err)
	}
	return true, nil
}
