//go:build integration

package queue_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/queue"
)

// newPool connects to the database the integration-test check brings up. It
// uses db.Connect rather than Open, so a database the migrations have not been
// run against fails here saying so: tests never build a schema of their own.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// kinds is what keeps tests out of each other's way. The queue table outlives
// one test and every operation is scoped to a kind, so a test takes a kind
// name nothing else uses rather than truncating a table another test is
// claiming from.
var kinds atomic.Int64

func newKind(t *testing.T, serialized bool) queue.Kind {
	t.Helper()
	name := "k" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(kinds.Add(1), 36)
	return queue.Kind{Name: name, Serialized: serialized}
}

// newClient returns a client on a kind of its own. Only Kind is taken from
// cfg's caller — the rest is theirs to set.
func newClient(t *testing.T, cfg queue.Config) *queue.Client {
	t.Helper()
	client, err := queue.New(newPool(t), cfg)
	if err != nil {
		t.Fatalf("queue.New = %v, want no error", err)
	}
	return client
}

func enqueue(t *testing.T, q queue.Querier, req queue.Request) queue.Enqueued {
	t.Helper()
	out, err := queue.Enqueue(t.Context(), q, req)
	if err != nil {
		t.Fatalf("Enqueue(%+v) = %v, want no error", req, err)
	}
	return out
}

func claim(t *testing.T, client *queue.Client) []queue.Job {
	t.Helper()
	jobs, err := client.Claim(t.Context())
	if err != nil {
		t.Fatalf("Claim = %v, want no error", err)
	}
	return jobs
}

func stats(t *testing.T, client *queue.Client) queue.Stats {
	t.Helper()
	s, err := client.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats = %v, want no error", err)
	}
	return s
}

// The acceptance criterion: a job is enqueued in the transaction that causes
// it, so a rollback takes the job with it. That is the whole reason the queue
// is in the same Postgres as the data (ADR-0007).
func TestEnqueueRollsBackWithTheTransactionThatWroteIt(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})

	tx, err := newPool(t).Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	out := enqueue(t, tx, queue.Request{Kind: kind, TargetID: "evt-1"})
	if !out.Stored {
		t.Fatal("Enqueue in a transaction did not store a job")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rolling back: %v", err)
	}

	if got := stats(t, client); got.Pending != 0 || got.Running != 0 {
		t.Errorf("after a rollback the queue holds %+v, want nothing", got)
	}
	if jobs := claim(t, client); len(jobs) != 0 {
		t.Errorf("Claim = %v, want nothing: the transaction that enqueued it rolled back", jobs)
	}
}

// The acceptance criterion: enqueueing twice for one target while a job is
// pending collapses, and — the part that matters — does not surface a unique
// violation into the caller's transaction, which would abort the L0 or L1
// write that was the point of it.
func TestADuplicateEnqueueCollapsesWithoutAbortingTheTransaction(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})
	pool := newPool(t)

	first := enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-1"})
	if !first.Stored {
		t.Fatal("the first Enqueue did not store a job")
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	defer tx.Rollback(context.Background())

	second := enqueue(t, tx, queue.Request{Kind: kind, TargetID: "evt-1"})
	if second.Stored || second.ID != 0 {
		t.Errorf("the second Enqueue = %+v, want it to collapse into the pending job", second)
	}

	// The transaction is still usable, which is what "never aborts the
	// caller's transaction" means: a failed statement would have poisoned it
	// and this query would report so.
	var one int
	if err := tx.QueryRow(t.Context(), `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("the transaction is unusable after a duplicate enqueue: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("committing after a duplicate enqueue: %v", err)
	}

	if got := stats(t, client).Pending; got != 1 {
		t.Errorf("pending = %d after enqueueing the same target twice, want 1", got)
	}
}

// Identity is only "pending", on purpose: once a job is running it may already
// have read the state a new event is about, so the new event is new work
// rather than the same work.
func TestEnqueueingForARunningJobIsANewJob(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})

	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	jobs := claim(t, client)
	if len(jobs) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(jobs))
	}

	second := enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	if !second.Stored {
		t.Error("enqueueing for a target whose job is running did not store a job, want a new one")
	}
	if got := stats(t, client); got.Pending != 1 || got.Running != 1 {
		t.Errorf("stats = %+v, want one pending and one running", got)
	}
}

// The acceptance criterion for unserialized kinds: workers claiming the same
// backlog step over each other's rows rather than taking them twice.
func TestConcurrentWorkersNeverClaimOneRowTwice(t *testing.T) {
	const (
		jobs    = 60
		workers = 6
	)
	kind := newKind(t, false)
	pool := newPool(t)
	for i := range jobs {
		enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-" + strconv.Itoa(i)})
	}

	clients := make([]*queue.Client, workers)
	for i := range clients {
		clients[i] = newClient(t, queue.Config{Kind: kind, Concurrency: 4})
	}

	var (
		mu      sync.Mutex
		claimed = map[int64]int{}
		wg      sync.WaitGroup
	)
	for _, client := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				batch, err := client.Claim(t.Context())
				if err != nil {
					t.Errorf("Claim = %v, want no error", err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, job := range batch {
					claimed[job.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != jobs {
		t.Errorf("%d distinct jobs were claimed, want all %d", len(claimed), jobs)
	}
	for id, times := range claimed {
		if times != 1 {
			t.Errorf("job %d was claimed %d times, want once", id, times)
		}
	}
	if got := stats(t, clients[0]); got.Running != jobs || got.Pending != 0 {
		t.Errorf("stats = %+v, want all %d jobs running", got, jobs)
	}
}

// The acceptance criterion ADR-0007 says must be tested directly: two jobs
// sharing a serial key are never running at the same time, however many
// workers are claiming.
func TestConcurrentWorkersNeverRunTwoJobsOfOneSerialKey(t *testing.T) {
	const (
		keys       = 3
		perKey     = 4
		workers    = 5
		jobRuntime = 15 * time.Millisecond
	)
	kind := newKind(t, true)
	pool := newPool(t)
	for k := range keys {
		for i := range perKey {
			enqueue(t, pool, queue.Request{
				Kind:      kind,
				TargetID:  "l1-" + strconv.Itoa(k) + "-" + strconv.Itoa(i),
				SerialKey: "scope-" + strconv.Itoa(k),
			})
		}
	}

	var (
		mu      sync.Mutex
		running = map[string]int{}
		clashes []string
		done    atomic.Int64
		wg      sync.WaitGroup
	)
	for range workers {
		client := newClient(t, queue.Config{Kind: kind, Lease: 30 * time.Second})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done.Load() < keys*perKey {
				batch, err := client.Claim(t.Context())
				if err != nil {
					t.Errorf("Claim = %v, want no error", err)
					return
				}
				if len(batch) == 0 {
					// Every key is busy, or the queue is empty; either way,
					// come back.
					time.Sleep(time.Millisecond)
					continue
				}
				if len(batch) != 1 {
					t.Errorf("a serialized claim took %d jobs, want at most 1", len(batch))
				}
				job := batch[0]

				mu.Lock()
				running[job.SerialKey]++
				if running[job.SerialKey] > 1 {
					clashes = append(clashes, job.SerialKey)
				}
				mu.Unlock()

				// Hold the key long enough that a claim which ignored the
				// invariant would overlap with this one.
				time.Sleep(jobRuntime)

				mu.Lock()
				running[job.SerialKey]--
				mu.Unlock()

				if ok, err := client.Complete(t.Context(), job); err != nil || !ok {
					t.Errorf("Complete(%s) = %v, %v, want true and no error", job, ok, err)
				}
				done.Add(1)
			}
		}()
	}
	wg.Wait()

	if len(clashes) > 0 {
		t.Errorf("two jobs of one serial key ran at once, on %v", clashes)
	}
	if got := done.Load(); got != keys*perKey {
		t.Errorf("%d jobs ran, want %d", got, keys*perKey)
	}
}

// The serialized claim's *other* guard, and the one a race between two workers
// only rarely reaches: the claim takes an advisory lock on its kind, so that
// two claims cannot both read a snapshot in which one serial key is free
// (ADR-0007). The window that needs is about a millisecond wide, which the
// test above hits by luck rather than by design — so the lock is held here by
// the test instead, and a claim that did not take it is caught every run.
//
// The lock's name is spelled out rather than taken from the package because it
// is a contract between workers and not an implementation detail: two workers
// hashing different strings would each serialize against nobody, and neither
// would notice.
func TestTheSerializedClaimTakesTheKindsAdvisoryLock(t *testing.T) {
	pool := newPool(t)
	serialized := newKind(t, true)
	unserialized := newKind(t, false)
	enqueue(t, pool, queue.Request{Kind: serialized, TargetID: "l1-a", SerialKey: "scope"})
	enqueue(t, pool, queue.Request{Kind: unserialized, TargetID: "evt-a"})

	// A connection of its own, because an advisory lock without `xact` in its
	// name belongs to a session: this one holds both kinds' claim locks until
	// the test hands them back.
	holder, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquiring a connection to hold the claim lock: %v", err)
	}
	t.Cleanup(func() {
		// Not t.Context(): it is cancelled before cleanup runs, and a lock
		// left behind would outlive the test on a pooled connection.
		_, _ = holder.Exec(context.Background(), `SELECT pg_advisory_unlock_all()`)
		holder.Release()
	})
	for _, kind := range []queue.Kind{serialized, unserialized} {
		if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_lock(hashtext('claim:' || $1))`, kind.Name); err != nil {
			t.Fatalf("taking the claim lock for %s: %v", kind.Name, err)
		}
	}

	// With the lock held the serialized claim cannot reach its UPDATE, so it
	// waits rather than deciding the key is free, and comes back with the
	// deadline it was given and no job.
	const wait = time.Second
	client := newClient(t, queue.Config{Kind: serialized})
	blocked, giveUp := context.WithTimeout(t.Context(), wait)
	defer giveUp()
	start := time.Now()
	jobs, err := client.Claim(blocked)
	waited := time.Since(start)
	if err == nil || len(jobs) != 0 {
		t.Fatalf("Claim with the kind's claim lock held = %v, %v; want it to block on the lock and take nothing", jobs, err)
	}
	if waited < wait/2 {
		t.Errorf("Claim came back after %s, want it to have waited on the lock until its context ran out at %s", waited, wait)
	}

	// The positive control. Only the serialized path takes this lock, so
	// holding an unserialized kind's claims nothing — without this, a Claim
	// that failed for some entirely other reason would read as the lock
	// working.
	if got := claim(t, newClient(t, queue.Config{Kind: unserialized})); len(got) != 1 {
		t.Fatalf("an unserialized claim with that kind's lock held = %v, want the one pending job: it does not take the lock", got)
	}

	// And once the lock is released, the job that was waiting on it is taken.
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatalf("releasing the claim locks: %v", err)
	}
	if got := claim(t, client); len(got) != 1 || got[0].TargetID != "l1-a" {
		t.Fatalf("after the lock was released, Claim = %v, want the pending job", got)
	}
}

// A serialized claim must not be starved by a key that is busy: the other
// keys keep running.
func TestASerializedClaimSkipsABusyKeyAndTakesAnother(t *testing.T) {
	kind := newKind(t, true)
	client := newClient(t, queue.Config{Kind: kind, Lease: 30 * time.Second})
	pool := newPool(t)
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "a1", SerialKey: "a"})
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "a2", SerialKey: "a"})
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "b1", SerialKey: "b"})

	first := claim(t, client)
	if len(first) != 1 || first[0].SerialKey != "a" {
		t.Fatalf("the first claim = %v, want the oldest job, on key a", first)
	}
	second := claim(t, client)
	if len(second) != 1 || second[0].SerialKey != "b" {
		t.Fatalf("the second claim = %v, want key b: key a is running", second)
	}
	if third := claim(t, client); len(third) != 0 {
		t.Fatalf("the third claim = %v, want nothing: both keys are running", third)
	}

	if ok, err := client.Complete(t.Context(), first[0]); err != nil || !ok {
		t.Fatalf("Complete = %v, %v", ok, err)
	}
	fourth := claim(t, client)
	if len(fourth) != 1 || fourth[0].TargetID != "a2" {
		t.Fatalf("after finishing a1, the claim = %v, want a2", fourth)
	}
}

// The acceptance criterion: a job that runs out of attempts lands in failed
// and stays there, queryable, with what went wrong on it.
func TestAJobThatRunsOutOfAttemptsFailsAndStays(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{
		Kind: kind, MaxAttempts: 2, Backoff: time.Second, MaxBackoff: time.Second,
	})
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

	first := claim(t, client)
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("the first claim = %v, want one job on attempt 1", first)
	}
	before := time.Now()
	state, retry, err := client.Fail(t.Context(), first[0], errors.New("the model timed out"))
	if err != nil {
		t.Fatalf("Fail = %v, want no error", err)
	}
	if state != queue.StatePending {
		t.Errorf("Fail on attempt 1 of 2 = %q, want the job back in %q", state, queue.StatePending)
	}
	if retry.In <= 0 || retry.In > time.Second || retry.At.IsZero() {
		t.Errorf("Fail returned retry %+v, want a delay inside the configured backoff and a time", retry)
	}
	// The delay is not only reported, it is when the job actually runs again.
	// The jitter can draw almost nothing, so this asserts run_after is the
	// delay that was drawn into the future — not that the delay is long.
	if earliest := before.Add(retry.In - 20*time.Millisecond); retry.At.Before(earliest) {
		t.Errorf("the retry is scheduled for %s but the backoff applied was %s from %s: run_after did not move",
			retry.At, retry.In, before)
	}

	// Wait out the backoff rather than assuming it has passed.
	var second []queue.Job
	for deadline := time.Now().Add(5 * time.Second); len(second) == 0 && time.Now().Before(deadline); {
		second = claim(t, client)
	}
	if len(second) != 1 || second[0].Attempt != 2 {
		t.Fatalf("the second claim = %v, want the same job on attempt 2", second)
	}
	if second[0].LastError != "the model timed out" {
		t.Errorf("the retried job's LastError = %q, want the first attempt's error", second[0].LastError)
	}

	state, retry, err = client.Fail(t.Context(), second[0], errors.New("the model timed out again"))
	if err != nil {
		t.Fatalf("Fail = %v, want no error", err)
	}
	if state != queue.StateFailed {
		t.Fatalf("Fail on the last attempt = %q, want %q", state, queue.StateFailed)
	}
	if retry != (queue.Retry{}) {
		t.Errorf("Fail on the last attempt returned retry %+v, want none", retry)
	}

	if jobs := claim(t, client); len(jobs) != 0 {
		t.Errorf("Claim = %v, want nothing: a failed job is not work", jobs)
	}
	failed, err := client.List(t.Context(), queue.StateFailed, 0)
	if err != nil {
		t.Fatalf("List(failed) = %v, want no error", err)
	}
	if len(failed) != 1 {
		t.Fatalf("List(failed) = %d jobs, want the one that ran out of attempts", len(failed))
	}
	if failed[0].LastError != "the model timed out again" || failed[0].Attempt != 2 || failed[0].FinishedAt.IsZero() {
		t.Errorf("the failed job = %+v, want its last error, its attempt count and a finish time", failed[0])
	}
	if got := stats(t, client); got.Failed != 1 || got.Pending != 0 || got.Running != 0 {
		t.Errorf("stats = %+v, want one failed job and nothing else", got)
	}
}

// A worker that dies leaves a running row. The lease is what turns it back
// into work, and it is why delivery is at-least-once.
func TestAnExpiredLeaseIsReclaimedAndRunAgain(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Lease: 100 * time.Millisecond})
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

	dead := claim(t, client)
	if len(dead) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(dead))
	}

	// Nothing heartbeats: this is the worker that died.
	var reclaimed queue.Reclaimed
	for deadline := time.Now().Add(5 * time.Second); reclaimed.Total() == 0 && time.Now().Before(deadline); {
		var err error
		if reclaimed, err = client.Reclaim(t.Context()); err != nil {
			t.Fatalf("Reclaim = %v, want no error", err)
		}
	}
	if reclaimed.Pending != 1 || reclaimed.Failed != 0 {
		t.Fatalf("Reclaim = %+v, want one job back in the queue", reclaimed)
	}

	again := claim(t, client)
	if len(again) != 1 || again[0].ID != dead[0].ID {
		t.Fatalf("Claim after a reclaim = %v, want the same job %d", again, dead[0].ID)
	}
	if again[0].Attempt != dead[0].Attempt+1 {
		t.Errorf("the reclaimed job is on attempt %d, want %d", again[0].Attempt, dead[0].Attempt+1)
	}

	// The worker that died must not be able to finish the run somebody else
	// is now doing.
	if ok, err := client.Complete(t.Context(), dead[0]); err != nil || ok {
		t.Errorf("Complete from the dead worker = %v, %v, want false and no error", ok, err)
	}
	if ok, err := client.Complete(t.Context(), again[0]); err != nil || !ok {
		t.Errorf("Complete from the worker that holds the job = %v, %v, want true", ok, err)
	}
}

// The other half of the lease: a job that is slow but alive is not taken away
// from the worker running it.
func TestAHeartbeatKeepsALongJobFromBeingReclaimed(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Lease: 300 * time.Millisecond})
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

	jobs := claim(t, client)
	if len(jobs) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(jobs))
	}

	// Longer than the lease, heartbeating as a worker would.
	for range 6 {
		time.Sleep(100 * time.Millisecond)
		held, err := client.Heartbeat(t.Context(), jobs[0])
		if err != nil {
			t.Fatalf("Heartbeat = %v, want no error", err)
		}
		if !held {
			t.Fatal("Heartbeat = false, want the lease to still be this run's")
		}
		reclaimed, err := client.Reclaim(t.Context())
		if err != nil {
			t.Fatalf("Reclaim = %v, want no error", err)
		}
		if reclaimed.Total() != 0 {
			t.Fatalf("Reclaim took %+v while the worker was heartbeating, want nothing", reclaimed)
		}
	}
}

// A job whose worker keeps dying must not be handed round for ever: the
// reclaimer fails it once its attempts are spent, the same as a handler that
// keeps returning an error.
func TestReclaimingFailsAJobThatHasNoAttemptsLeft(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Lease: 50 * time.Millisecond, MaxAttempts: 1})
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

	if jobs := claim(t, client); len(jobs) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(jobs))
	}
	var reclaimed queue.Reclaimed
	for deadline := time.Now().Add(5 * time.Second); reclaimed.Total() == 0 && time.Now().Before(deadline); {
		var err error
		if reclaimed, err = client.Reclaim(t.Context()); err != nil {
			t.Fatalf("Reclaim = %v, want no error", err)
		}
	}
	if reclaimed.Failed != 1 || reclaimed.Pending != 0 {
		t.Errorf("Reclaim = %+v, want the job failed: it had one attempt and used it", reclaimed)
	}
	if got := stats(t, client); got.Failed != 1 {
		t.Errorf("stats = %+v, want one failed job", got)
	}
}

// Retention: done jobs go, failed jobs stay. A failed job is the record of
// what did not happen.
func TestPurgeDeletesDoneJobsAndKeepsFailedOnes(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Concurrency: 2, MaxAttempts: 1, Retention: time.Nanosecond})
	pool := newPool(t)
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "done-1"})
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "failed-1"})

	claimed := claim(t, client)
	if len(claimed) != 2 {
		t.Fatalf("Claim = %d jobs, want both", len(claimed))
	}
	for _, job := range claimed {
		var err error
		var ok bool
		if job.TargetID == "done-1" {
			ok, err = client.Complete(t.Context(), job)
		} else {
			var state queue.State
			state, _, err = client.Fail(t.Context(), job, errors.New("nope"))
			ok = state == queue.StateFailed
		}
		if err != nil || !ok {
			t.Fatalf("reporting the outcome of %s = %v, %v", job, ok, err)
		}
	}

	// A listing is of one state, which is what makes a failed job findable
	// among the finished ones rather than alongside them.
	for state, want := range map[queue.State]string{queue.StateDone: "done-1", queue.StateFailed: "failed-1"} {
		listed, err := client.List(t.Context(), state, 0)
		if err != nil {
			t.Fatalf("List(%s) = %v, want no error", state, err)
		}
		if len(listed) != 1 || listed[0].TargetID != want || listed[0].State != state {
			t.Errorf("List(%s) = %+v, want only %s", state, listed, want)
		}
	}

	// Retention is a nanosecond, so both are past it; only the done one may go.
	purged, err := client.Purge(t.Context())
	if err != nil {
		t.Fatalf("Purge = %v, want no error", err)
	}
	if purged != 1 {
		t.Errorf("Purge deleted %d jobs, want the one that is done", purged)
	}
	got := stats(t, client)
	if got.Done != 0 || got.Failed != 1 {
		t.Errorf("stats = %+v, want the failed job kept and the done one gone", got)
	}
}

// A retention window that has not passed keeps the job.
func TestPurgeKeepsDoneJobsInsideTheRetentionWindow(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Retention: time.Hour})
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	for _, job := range claim(t, client) {
		if ok, err := client.Complete(t.Context(), job); err != nil || !ok {
			t.Fatalf("Complete = %v, %v", ok, err)
		}
	}
	purged, err := client.Purge(t.Context())
	if err != nil {
		t.Fatalf("Purge = %v, want no error", err)
	}
	if purged != 0 {
		t.Errorf("Purge deleted %d jobs, want none: they finished a moment ago", purged)
	}
	if got := stats(t, client).Done; got != 1 {
		t.Errorf("done = %d, want the job kept", got)
	}
}

// Scheduling is FIFO within a kind, and priority is what jumps the queue.
// Jobs enqueued in one transaction share run_after to the microsecond, so
// something has to break that tie for FIFO to mean anything.
func TestClaimTakesJobsByPriorityThenInOrder(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Concurrency: 2})

	tx, err := newPool(t).Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	for _, req := range []queue.Request{
		{Kind: kind, TargetID: "first"},
		{Kind: kind, TargetID: "second"},
		{Kind: kind, TargetID: "third"},
		{Kind: kind, TargetID: "urgent", Priority: 10},
	} {
		enqueue(t, tx, req)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("committing: %v", err)
	}

	// Two claims of two, so the test pins which jobs a claim takes and not
	// only how the batch it took is sorted.
	for _, want := range [][]string{{"urgent", "first"}, {"second", "third"}} {
		var order []string
		for _, job := range claim(t, client) {
			order = append(order, job.TargetID)
		}
		if !slices.Equal(order, want) {
			t.Fatalf("Claim = %v, want %v", order, want)
		}
	}
}

// A delayed job is not work until it is due, and the delay is measured by
// Postgres rather than by the enqueuing process's clock.
func TestADelayedJobIsNotClaimedBeforeItIsDue(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1", Delay: 400 * time.Millisecond})

	if jobs := claim(t, client); len(jobs) != 0 {
		t.Fatalf("Claim = %v, want nothing: the job is not due", jobs)
	}
	if got := stats(t, client); got.Pending != 1 || got.OldestPending != 0 {
		t.Errorf("stats = %+v, want one pending job that is not yet due", got)
	}

	var jobs []queue.Job
	for deadline := time.Now().Add(5 * time.Second); len(jobs) == 0 && time.Now().Before(deadline); {
		jobs = claim(t, client)
	}
	if len(jobs) != 1 {
		t.Fatal("the delayed job never became claimable")
	}
}

// What the enqueuing side attaches to a job comes back to the worker: that is
// how the work joins the trace that caused it (ADR-0008).
func TestTheTraceContextReachesTheWorker(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})
	carrier := map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1", TraceContext: carrier})

	jobs := claim(t, client)
	if len(jobs) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(jobs))
	}
	if got := jobs[0].TraceContext["traceparent"]; got != carrier["traceparent"] {
		t.Errorf("the claimed job's traceparent = %q, want %q", got, carrier["traceparent"])
	}

	// A job with nothing attached comes back with an empty carrier rather
	// than a null the worker has to guard.
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-2"})
	plain := claim(t, client)
	if len(plain) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(plain))
	}
	if plain[0].TraceContext == nil || len(plain[0].TraceContext) != 0 {
		t.Errorf("a job enqueued with no trace context has %v, want an empty carrier", plain[0].TraceContext)
	}
}

// A handler's error is written to the row and logged, so it is bounded: an
// error that carried a whole document would put the document in both
// (ADR-0008). The cut is on a rune boundary, which Postgres would refuse if it
// were not — text is UTF-8 and half a rune is not.
func TestAnOversizedErrorIsStoredBoundedAndStillValidText(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, MaxAttempts: 1})
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

	// A three-byte rune, so that a cut taken at a byte count lands inside one.
	huge := strings.Repeat("☃", 5000)
	jobs := claim(t, client)
	if len(jobs) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(jobs))
	}
	if state, _, err := client.Fail(t.Context(), jobs[0], errors.New(huge)); err != nil || state != queue.StateFailed {
		t.Fatalf("Fail = %q, %v, want the job failed and no error", state, err)
	}

	failed, err := client.List(t.Context(), queue.StateFailed, 0)
	if err != nil || len(failed) != 1 {
		t.Fatalf("List(failed) = %v, %v, want the failed job", failed, err)
	}
	stored := failed[0].LastError
	if len(stored) >= len(huge) {
		t.Errorf("the stored error is %d bytes, want it bounded well below the %d it was given", len(stored), len(huge))
	}
	if !utf8.ValidString(stored) {
		t.Error("the stored error is not valid UTF-8: the bound cut a rune in half")
	}
	if !strings.HasPrefix(huge, strings.TrimSuffix(stored, "…")) {
		t.Error("the stored error is not the beginning of the error that was reported")
	}
}

// The defaults are filled in when the client is built, so a service that
// configures nothing still has the numbers ADR-0007 describes.
func TestAClientFillsInTheDefaults(t *testing.T) {
	client := newClient(t, queue.Config{Kind: newKind(t, false)})
	got := client.Config()
	if got.Concurrency != 1 || got.Lease != queue.DefaultLease || got.MaxAttempts != queue.DefaultMaxAttempts ||
		got.PollInterval != queue.DefaultPollInterval || got.Retention != queue.DefaultRetention {
		t.Errorf("Config() = %+v, want the defaults filled in", got)
	}
}

// The table is the last line of defence, and these are the rows the package's
// own API cannot produce — which is the point of asserting them here: a
// constraint is what stops a path added later from producing one quietly. It
// is also the only place a test writes the queue tables directly.
func TestTheSchemaRefusesRowsTheQueueWouldNeverWrite(t *testing.T) {
	kind := newKind(t, false)
	pool := newPool(t)

	const insert = `
INSERT INTO queue_job (kind, target_id, serial_key, state, attempt, started_at, lease_expires_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	now := time.Now()
	noKey := ""
	tests := []struct {
		name string
		// The row, column by column, so that every case inserts a whole one
		// and a case can only fail on the constraint it is about.
		serialKey  *string
		state      string
		attempt    int
		startedAt  *time.Time
		leaseUntil *time.Time
		finishedAt *time.Time
		// wantCode is the SQLSTATE, and wantName the constraint or column it
		// names. Empty wantCode is a row the table must accept.
		wantCode string
		wantName string
	}{
		{name: "a pending job", serialKey: &noKey, state: "pending"},
		{name: "a running job with a lease", serialKey: &noKey, state: "running", attempt: 1, startedAt: &now, leaseUntil: &now},
		{name: "a finished job", serialKey: &noKey, state: "done", attempt: 1, startedAt: &now, finishedAt: &now},
		{
			name: "a null serial key", serialKey: nil, state: "pending",
			wantCode: "23502", wantName: "serial_key",
		},
		{
			name: "a state nothing knows", serialKey: &noKey, state: "claimed",
			wantCode: "23514", wantName: "queue_job_state_is_known",
		},
		{
			name: "a running job with no lease", serialKey: &noKey, state: "running", attempt: 1, startedAt: &now,
			wantCode: "23514", wantName: "queue_job_a_lease_is_a_running_job",
		},
		{
			name: "a lease on a job nobody is running", serialKey: &noKey, state: "pending", leaseUntil: &now,
			wantCode: "23514", wantName: "queue_job_a_lease_is_a_running_job",
		},
		{
			name: "a pending job part-way through a run", serialKey: &noKey, state: "pending", startedAt: &now,
			wantCode: "23514", wantName: "queue_job_pending_is_not_started",
		},
		{
			name: "a finished job with no finish time", serialKey: &noKey, state: "done", attempt: 1, startedAt: &now,
			wantCode: "23514", wantName: "queue_job_finished_is_terminal",
		},
		{
			name: "a finish time on a job that is still queued", serialKey: &noKey, state: "pending", finishedAt: &now,
			wantCode: "23514", wantName: "queue_job_finished_is_terminal",
		},
		{
			name: "a negative attempt count", serialKey: &noKey, state: "pending", attempt: -1,
			wantCode: "23514", wantName: "queue_job_attempt_is_not_negative",
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(t.Context(), insert,
				kind.Name, "target-"+strconv.Itoa(i), tt.serialKey, tt.state, tt.attempt,
				tt.startedAt, tt.leaseUntil, tt.finishedAt)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("inserting a row the queue does write = %v, want no error", err)
				}
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("inserting %s = %v, want a Postgres error", tt.name, err)
			}
			// Both, so that a case cannot pass on some other error: a
			// statement that is simply malformed reports neither of these.
			if pgErr.Code != tt.wantCode {
				t.Errorf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, tt.wantCode)
			}
			if name := pgErr.ConstraintName + pgErr.ColumnName; name != tt.wantName {
				t.Errorf("the error names %q, want %q", name, tt.wantName)
			}
		})
	}
}
