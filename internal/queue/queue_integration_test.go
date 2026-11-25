//go:build integration

package queue_test

import (
	"context"
	"errors"
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

	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/poll"
	"github.com/kpenfound/hearsay/internal/queue"
)

var kinds atomic.Int64

func newKind(t *testing.T, serialized bool) queue.Kind {
	t.Helper()
	name := "k" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(kinds.Add(1), 36)
	return queue.Kind{Name: name, Serialized: serialized}
}

func newClient(t *testing.T, cfg queue.Config) *queue.Client {
	t.Helper()
	client, err := queue.New(dbtest.Pool(t), cfg)
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

func TestEnqueueRollsBackWithTheTransactionThatWroteIt(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})

	tx, err := dbtest.Pool(t).Begin(t.Context())
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

func TestADuplicateEnqueueCollapsesWithoutAbortingTheTransaction(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})
	pool := dbtest.Pool(t)

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

func TestDebouncedEnqueue(t *testing.T) {
	tests := []struct {
		name       string
		firstQuiet time.Duration
		next       *queue.Debounce
		running    bool
	}{
		{name: "new target"},
		{name: "running target", running: true},
		{name: "pushes later", firstQuiet: time.Hour, next: &queue.Debounce{Quiet: time.Hour, Max: 3 * time.Hour}},
		{name: "keeps a later schedule", firstQuiet: 2 * time.Hour, next: &queue.Debounce{Quiet: time.Hour, Max: 3 * time.Hour}},
		{name: "caps at first enqueue plus max", firstQuiet: time.Hour, next: &queue.Debounce{Quiet: 2 * time.Hour, Max: 2 * time.Hour}},
		{name: "non-debounced duplicate does nothing", firstQuiet: time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := newKind(t, false)
			pool := dbtest.Pool(t)
			client := newClient(t, queue.Config{Kind: kind})
			var before queue.Job
			if tt.firstQuiet != 0 || tt.running {
				req := queue.Request{Kind: kind, TargetID: "doc-1", Priority: 7, TraceContext: map[string]string{"traceparent": "original"}}
				if !tt.running {
					req.Debounce = &queue.Debounce{Quiet: tt.firstQuiet, Max: 3 * time.Hour}
				}
				enqueue(t, pool, req)
				if tt.running {
					if jobs := claim(t, client); len(jobs) != 1 {
						t.Fatalf("Claim = %v, want one running job", jobs)
					}
				} else {
					before = listJobs(t, client, queue.StatePending)[0]
				}
			}

			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(t.Context()))
			var now time.Time
			if err := tx.QueryRow(t.Context(), "SELECT now()").Scan(&now); err != nil {
				t.Fatal(err)
			}
			db := tt.next
			if tt.firstQuiet == 0 {
				db = &queue.Debounce{Quiet: time.Minute, Max: time.Hour}
			}
			out := enqueue(t, tx, queue.Request{Kind: kind, TargetID: "doc-1", Debounce: db})
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			jobs := listJobs(t, client, queue.StatePending)
			if len(jobs) != 1 {
				t.Fatalf("List(pending) = %v, want one job", jobs)
			}
			got := jobs[0]
			wantEnqueued, wantRun := now, now.Add(time.Minute)
			if before.ID != 0 {
				wantEnqueued, wantRun = before.EnqueuedAt, before.RunAfter
				if db != nil {
					if candidate := now.Add(db.Quiet); candidate.After(wantRun) {
						wantRun = candidate
					}
					if capAt := before.EnqueuedAt.Add(db.Max); capAt.Before(wantRun) {
						wantRun = capAt
					}
				}
				if got.ID != before.ID || got.Priority != before.Priority || got.TraceContext["traceparent"] != "original" {
					t.Errorf("job metadata changed: before %+v, after %+v", before, got)
				}
			}
			if !got.EnqueuedAt.Equal(wantEnqueued) || !got.RunAfter.Equal(wantRun) {
				t.Errorf("schedule = (%s, %s), want (%s, %s)", got.EnqueuedAt, got.RunAfter, wantEnqueued, wantRun)
			}
			if db == nil {
				if out.Stored || out.ID != 0 {
					t.Errorf("non-debounced duplicate = %+v, want no write", out)
				}
			} else if !out.Stored || out.ID != got.ID {
				t.Errorf("debounced Enqueue = %+v, want Stored with ID %d", out, got.ID)
			}
			if jobs := claim(t, client); len(jobs) != 0 {
				t.Errorf("Claim = %v, want no jobs before quiet period", jobs)
			}
			if tt.running && stats(t, client).Running != 1 {
				t.Error("debounced enqueue changed the running job")
			}
		})
	}
}

func TestADebouncedEnqueueKeepsADueJobDue(t *testing.T) {
	tests := []struct {
		name     string
		expedite bool
		oldTx    bool
		expired  bool
		capped   bool
	}{
		{name: "non-debounced insert"},
		{name: "expedited", expedite: true},
		{name: "transaction began before expedite", expedite: true, oldTx: true},
		{name: "quiet period elapsed", expired: true},
		{name: "maximum reached", expired: true, capped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := newKind(t, false)
			pool := dbtest.Pool(t)
			client := newClient(t, queue.Config{Kind: kind})
			req := queue.Request{Kind: kind, TargetID: "doc-1"}
			if tt.expedite {
				req.Debounce = &queue.Debounce{Quiet: time.Hour, Max: 2 * time.Hour}
			} else if tt.expired {
				req.Debounce = &queue.Debounce{Quiet: 500 * time.Millisecond, Max: time.Second}
			}
			enqueue(t, pool, req)
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(t.Context()))
			if _, err := tx.Exec(t.Context(), "SELECT now()"); err != nil {
				t.Fatal(err)
			}
			if tt.capped {
				enqueue(t, pool, queue.Request{Kind: kind, TargetID: "doc-1", Debounce: &queue.Debounce{Quiet: time.Second, Max: time.Second}})
				job := listJobs(t, client, queue.StatePending)[0]
				if !job.RunAfter.Equal(job.EnqueuedAt.Add(time.Second)) {
					t.Fatal("test setup did not reach the maximum")
				}
			}
			if tt.expedite {
				if ok, err := client.Expedite(t.Context(), kind, "doc-1"); err != nil || !ok {
					t.Fatalf("Expedite = %v, %v, want true, nil", ok, err)
				}
			}
			if tt.expired {
				if _, err := pool.Exec(t.Context(),
					`UPDATE queue_job SET enqueued_at = enqueued_at - interval '2 seconds', run_after = run_after - interval '2 seconds' WHERE kind = $1`, kind.Name); err != nil {
					t.Fatal(err)
				}
			}
			before := listJobs(t, client, queue.StatePending)[0]
			if !tt.oldTx {
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				tx, err = pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.WithoutCancel(t.Context()))
			}
			req.Debounce = &queue.Debounce{Quiet: time.Hour, Max: 2 * time.Hour}
			if out := enqueue(t, tx, req); out.Stored || out.ID != 0 {
				t.Errorf("Enqueue of due target = %+v, want no update", out)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			after := listJobs(t, client, queue.StatePending)[0]
			if !after.RunAfter.Equal(before.RunAfter) || !after.EnqueuedAt.Equal(before.EnqueuedAt) {
				t.Errorf("due schedule moved: before %+v, after %+v", before, after)
			}
			if jobs := claim(t, client); len(jobs) != 1 || jobs[0].ID != before.ID {
				t.Errorf("Claim = %v, want the due job immediately", jobs)
			}
		})
	}
}

func TestADebouncedEnqueueWaitingOnExpediteKeepsTheJobDue(t *testing.T) {
	kind := newKind(t, false)
	pool := dbtest.Pool(t)
	client := newClient(t, queue.Config{Kind: kind})
	req := queue.Request{Kind: kind, TargetID: "doc-1", Debounce: &queue.Debounce{Quiet: time.Hour, Max: 2 * time.Hour}}
	enqueue(t, pool, req)
	enqueuer, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer enqueuer.Rollback(context.WithoutCancel(t.Context()))
	if _, err := enqueuer.Exec(t.Context(), "SELECT now()"); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(t.Context()))
	var blocker int
	if err := tx.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	if ok, err := queue.Expedite(t.Context(), tx, kind, "doc-1"); err != nil || !ok {
		t.Fatalf("Expedite = %v, %v, want true, nil", ok, err)
	}
	type result struct {
		out queue.Enqueued
		err error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		defer close(finished)
		out, err := queue.Enqueue(ctx, enqueuer, req)
		done <- result{out, err}
	}()
	defer func() {
		cancel()
		<-finished
	}()
	poll.Until(t, "debounced enqueue to wait on expedite", func() bool {
		var blocked bool
		err := pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))", blocker).Scan(&blocked)
		return err == nil && blocked
	})
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.out.Stored {
		t.Errorf("waiting Enqueue = %+v, %v, want no update", got.out, got.err)
	}
	if err := enqueuer.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if jobs := claim(t, client); len(jobs) != 1 {
		t.Errorf("Claim = %v, want the expedited job immediately", jobs)
	}
}

func TestExpedite(t *testing.T) {
	for _, form := range []string{"transaction", "client"} {
		t.Run(form, func(t *testing.T) {
			for _, state := range []string{"delayed", "due", "missing", "running", "done", "rollback"} {
				t.Run(state, func(t *testing.T) {
					kind := newKind(t, false)
					pool := dbtest.Pool(t)
					client := newClient(t, queue.Config{Kind: kind})
					if state != "missing" {
						req := queue.Request{Kind: kind, TargetID: "doc-1"}
						if state == "delayed" || state == "rollback" {
							req.Delay = time.Hour
						}
						enqueue(t, pool, req)
						if state == "running" || state == "done" {
							jobs := claim(t, client)
							if len(jobs) != 1 {
								t.Fatalf("Claim = %v, want one job", jobs)
							}
							if state == "done" {
								if ok, err := client.Complete(t.Context(), jobs[0]); err != nil || !ok {
									t.Fatalf("Complete = %v, %v, want true, nil", ok, err)
								}
							}
						}
					}
					enqueue(t, pool, queue.Request{Kind: kind, TargetID: "other-doc", Delay: time.Hour})
					otherKind := newKind(t, false)
					enqueue(t, pool, queue.Request{Kind: otherKind, TargetID: "doc-1", Delay: time.Hour})
					other := newClient(t, queue.Config{Kind: otherKind})
					beforeOther := listJobs(t, other, queue.StatePending)[0]

					listener, err := pool.Acquire(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Release()
					if _, err := listener.Exec(t.Context(), "LISTEN "+kind.Channel()); err != nil {
						t.Fatal(err)
					}
					expectNotification := func(want bool) {
						t.Helper()
						timeout := 30 * time.Millisecond
						if want {
							timeout = time.Second
						}
						ctx, cancel := context.WithTimeout(t.Context(), timeout)
						defer cancel()
						n, err := listener.Conn().WaitForNotification(ctx)
						if want {
							if err != nil || n.Channel != kind.Channel() {
								t.Fatalf("notification = %v, %v, want channel %s", n, err, kind.Channel())
							}
						} else if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("notification = %v, %v, want none", n, err)
						}
					}
					want := state == "delayed" || state == "due" || state == "rollback"
					var ok bool
					if form == "client" {
						caller, beginErr := pool.Begin(t.Context())
						if beginErr != nil {
							t.Fatal(beginErr)
						}
						defer caller.Rollback(context.WithoutCancel(t.Context()))
						ok, err = other.Expedite(t.Context(), kind, "doc-1")
						if err := caller.Rollback(t.Context()); err != nil {
							t.Fatal(err)
						}
					} else {
						tx, beginErr := pool.Begin(t.Context())
						if beginErr != nil {
							t.Fatal(beginErr)
						}
						defer tx.Rollback(context.WithoutCancel(t.Context()))
						ok, err = queue.Expedite(t.Context(), tx, kind, "doc-1")
						expectNotification(false)
						if state == "rollback" {
							if err := tx.Rollback(t.Context()); err != nil {
								t.Fatal(err)
							}
						} else if err := tx.Commit(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					if err != nil || ok != want {
						t.Fatalf("Expedite = %v, %v, want %v, nil", ok, err, want)
					}
					expectNotification(want && (state != "rollback" || form == "client"))
					wantJobs := 0
					if want && (state != "rollback" || form == "client") {
						wantJobs = 1
					}
					if jobs := claim(t, client); len(jobs) != wantJobs || (wantJobs == 1 && jobs[0].TargetID != "doc-1") {
						t.Errorf("Claim = %v, want %d expedited jobs", jobs, wantJobs)
					}
					afterOther := listJobs(t, other, queue.StatePending)[0]
					if !beforeOther.RunAfter.Equal(afterOther.RunAfter) {
						t.Error("Expedite changed another kind's job")
					}
					if state == "running" && stats(t, client).Running != 1 {
						t.Error("Expedite changed the running job")
					}
					if state == "done" && stats(t, client).Done != 1 {
						t.Error("Expedite changed the completed job")
					}
				})
			}
		})
	}
}

func TestADebouncedUpdateNotifiesOnCommit(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(strconv.FormatBool(commit), func(t *testing.T) {
			kind := newKind(t, false)
			pool := dbtest.Pool(t)
			client := newClient(t, queue.Config{Kind: kind})
			req := queue.Request{Kind: kind, TargetID: "doc-1", Debounce: &queue.Debounce{Quiet: time.Hour, Max: 3 * time.Hour}}
			enqueue(t, pool, req)
			before := listJobs(t, client, queue.StatePending)[0]
			listener, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Release()
			if _, err := listener.Exec(t.Context(), "LISTEN "+kind.Channel()); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(t.Context()))
			req.Debounce = &queue.Debounce{Quiet: 2 * time.Hour, Max: 3 * time.Hour}
			enqueue(t, tx, req)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			if n, err := listener.Conn().WaitForNotification(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("notification before commit = %v, %v, want none", n, err)
			}
			if commit {
				err = tx.Commit(t.Context())
			} else {
				err = tx.Rollback(t.Context())
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel = context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			n, err := listener.Conn().WaitForNotification(ctx)
			if commit {
				if err != nil || n.Channel != kind.Channel() {
					t.Fatalf("notification after commit = %v, %v, want channel %s", n, err, kind.Channel())
				}
			} else if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("notification after rollback = %v, %v, want none", n, err)
			}
			after := listJobs(t, client, queue.StatePending)[0]
			if commit && !after.RunAfter.After(before.RunAfter) {
				t.Error("committed debounce update did not postpone the job")
			} else if !commit && !after.RunAfter.Equal(before.RunAfter) {
				t.Error("rolled-back debounce update changed the schedule")
			}
		})
	}
}

func TestARefusedEnqueueLeavesTheCallersTransactionUsable(t *testing.T) {
	kind := newKind(t, false)
	pool := dbtest.Pool(t)

	tests := []struct {
		name string
		req  queue.Request
	}{
		{name: "debounce with delay", req: queue.Request{Kind: kind, TargetID: "evt-1", Delay: time.Second, Debounce: &queue.Debounce{Quiet: time.Second, Max: time.Minute}}},
		{name: "zero quiet", req: queue.Request{Kind: kind, TargetID: "evt-1", Debounce: &queue.Debounce{Max: time.Minute}}},
		{name: "negative quiet", req: queue.Request{Kind: kind, TargetID: "evt-1", Debounce: &queue.Debounce{Quiet: -time.Second, Max: time.Minute}}},
		{name: "maximum below quiet", req: queue.Request{Kind: kind, TargetID: "evt-1", Debounce: &queue.Debounce{Quiet: time.Second, Max: time.Millisecond}}},
		{name: "a NUL in the target id", req: queue.Request{Kind: kind, TargetID: "evt-\x00-1"}},
		{name: "invalid UTF-8 in the target id", req: queue.Request{Kind: kind, TargetID: "evt-\xff"}},
		{name: "a target id past the limit", req: queue.Request{Kind: kind, TargetID: strings.Repeat("e", queue.MaxIDLen+1)}},
		{name: "a NUL in the trace context", req: queue.Request{Kind: kind, TargetID: "evt-carrier", TraceContext: map[string]string{"traceparent": "00-\x00"}}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("Begin = %v, want no error", err)
			}
			defer tx.Rollback(context.WithoutCancel(t.Context())) //nolint:errcheck // a rollback after commit is a no-op

			if _, err := queue.Enqueue(t.Context(), tx, tt.req); err == nil {
				t.Fatal("Enqueue accepted an invalid request")
			} else if !errors.Is(err, queue.ErrInvalidJob) {
				t.Fatalf("Enqueue = %v, want it to wrap ErrInvalidJob rather than come back from the database", err)
			}

			survivor := queue.Request{Kind: kind, TargetID: "evt-survivor-" + strconv.Itoa(i)}
			if _, err := queue.Enqueue(t.Context(), tx, survivor); err != nil {
				t.Fatalf("enqueueing after a refused request = %v, want the caller's transaction still usable", err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatalf("Commit = %v, want the transaction the enqueue was refused in to commit", err)
			}
		})
	}

	client := newClient(t, queue.Config{Kind: kind})
	pending, err := client.List(t.Context(), queue.StatePending, 0)
	if err != nil {
		t.Fatalf("List = %v, want no error", err)
	}
	if len(pending) != len(tests) {
		t.Errorf("%d jobs committed, want %d: one per transaction an enqueue was refused in", len(pending), len(tests))
	}
}

func TestEnqueueingForARunningJobIsANewJob(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})

	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	jobs := claim(t, client)
	if len(jobs) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(jobs))
	}

	second := enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	if !second.Stored {
		t.Error("enqueueing for a target whose job is running did not store a job, want a new one")
	}
	if got := stats(t, client); got.Pending != 1 || got.Running != 1 {
		t.Errorf("stats = %+v, want one pending and one running", got)
	}
}

func TestConcurrentWorkersNeverClaimOneRowTwice(t *testing.T) {
	const (
		jobs    = 60
		workers = 6
	)
	kind := newKind(t, false)
	pool := dbtest.Pool(t)
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

func TestConcurrentWorkersNeverRunTwoJobsOfOneSerialKey(t *testing.T) {
	const (
		keys       = 3
		perKey     = 4
		workers    = 5
		jobRuntime = 15 * time.Millisecond
	)
	kind := newKind(t, true)
	pool := dbtest.Pool(t)
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

func TestTheSerializedClaimTakesTheKindsAdvisoryLock(t *testing.T) {
	pool := dbtest.Pool(t)
	serialized := newKind(t, true)
	unserialized := newKind(t, false)
	enqueue(t, pool, queue.Request{Kind: serialized, TargetID: "l1-a", SerialKey: "scope"})
	enqueue(t, pool, queue.Request{Kind: unserialized, TargetID: "evt-a"})

	holder, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquiring a connection to hold the claim lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = holder.Exec(context.Background(), `SELECT pg_advisory_unlock_all()`)
		holder.Release()
	})
	for _, kind := range []queue.Kind{serialized, unserialized} {
		if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_lock(hashtext('claim:' || $1))`, kind.Name); err != nil {
			t.Fatalf("taking the claim lock for %s: %v", kind.Name, err)
		}
	}

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

	if got := claim(t, newClient(t, queue.Config{Kind: unserialized})); len(got) != 1 {
		t.Fatalf("an unserialized claim with that kind's lock held = %v, want the one pending job: it does not take the lock", got)
	}

	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatalf("releasing the claim locks: %v", err)
	}
	if got := claim(t, client); len(got) != 1 || got[0].TargetID != "l1-a" {
		t.Fatalf("after the lock was released, Claim = %v, want the pending job", got)
	}
}

func repeatableReadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbtest.URL(t))
	if err != nil {
		t.Fatalf("parsing the database url: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	var level string
	if err := pool.QueryRow(context.Background(), `SHOW default_transaction_isolation`).Scan(&level); err != nil {
		t.Fatalf("reading the pool's default isolation level: %v", err)
	}
	if level != "repeatable read" {
		t.Fatalf("the pool's default isolation level is %q, want repeatable read", level)
	}
	return pool
}

func TestTheSerializedClaimRefusesABusyKeyOnARepeatableReadServer(t *testing.T) {
	kind := newKind(t, true)
	pool := repeatableReadPool(t)
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "l1-waiting", SerialKey: "scope"})

	client, err := queue.New(pool, queue.Config{Kind: kind, Lease: 30 * time.Second})
	if err != nil {
		t.Fatalf("queue.New = %v, want no error", err)
	}

	holder, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquiring a connection to hold the claim lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = holder.Exec(context.Background(), `SELECT pg_advisory_unlock_all()`)
		holder.Release()
	})
	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_lock(hashtext('claim:' || $1))`, kind.Name); err != nil {
		t.Fatalf("taking the claim lock for %s: %v", kind.Name, err)
	}

	type claimResult struct {
		jobs []queue.Job
		err  error
	}
	claimed := make(chan claimResult, 1)
	go func() {
		jobs, err := client.Claim(context.WithoutCancel(t.Context()))
		claimed <- claimResult{jobs, err}
	}()

	poll.Until(t, "the serialized claim to block on the kind's advisory lock", func() bool {
		var waiting bool
		err := pool.QueryRow(t.Context(), `
SELECT count(*) > 0 FROM pg_locks
 WHERE locktype = 'advisory' AND NOT granted
   AND classid::bigint = ((hashtext('claim:' || $1)::bigint >> 32) & 4294967295)
   AND objid::bigint = (hashtext('claim:' || $1)::bigint & 4294967295)`, kind.Name).Scan(&waiting)
		if err != nil {
			t.Errorf("reading pg_locks = %v, want no error", err)
			return true
		}
		return waiting
	})

	if _, err := pool.Exec(t.Context(), `
INSERT INTO queue_job (kind, target_id, serial_key, state, attempt, started_at, lease_expires_at)
VALUES ($1, 'l1-running', 'scope', 'running', 1, now(), now() + interval '30 seconds')`, kind.Name); err != nil {
		t.Fatalf("starting a job on the serial key: %v", err)
	}

	if _, err := holder.Exec(t.Context(), `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatalf("releasing the claim lock: %v", err)
	}

	select {
	case got := <-claimed:
		if got.err != nil {
			t.Fatalf("Claim = %v, want no error", got.err)
		}
		if len(got.jobs) != 0 {
			t.Fatalf("Claim took %v while another job of serial key %q was running: the claim read a snapshot from before that job started, which is what READ COMMITTED is required for", got.jobs, "scope")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the claim never returned after the lock was released")
	}
}

func TestASerializedClaimSkipsABusyKeyAndTakesAnother(t *testing.T) {
	kind := newKind(t, true)
	client := newClient(t, queue.Config{Kind: kind, Lease: 30 * time.Second})
	pool := dbtest.Pool(t)
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

func TestAJobThatRunsOutOfAttemptsFailsAndStays(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{
		Kind: kind, MaxAttempts: 2, Backoff: time.Second, MaxBackoff: time.Second,
	})
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

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
	if earliest := before.Add(retry.In - 20*time.Millisecond); retry.At.Before(earliest) {
		t.Errorf("the retry is scheduled for %s but the backoff applied was %s from %s: run_after did not move",
			retry.At, retry.In, before)
	}

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

func TestAnExpiredLeaseIsReclaimedAndRunAgain(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Lease: 100 * time.Millisecond})
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

	dead := claim(t, client)
	if len(dead) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(dead))
	}

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

	if ok, err := client.Complete(t.Context(), dead[0]); err != nil || ok {
		t.Errorf("Complete from the dead worker = %v, %v, want false and no error", ok, err)
	}
	if ok, err := client.Complete(t.Context(), again[0]); err != nil || !ok {
		t.Errorf("Complete from the worker that holds the job = %v, %v, want true", ok, err)
	}
}

func TestAHeartbeatKeepsALongJobFromBeingReclaimed(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Lease: 300 * time.Millisecond})
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

	jobs := claim(t, client)
	if len(jobs) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(jobs))
	}

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

func TestReclaimingFailsAJobThatHasNoAttemptsLeft(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Lease: 50 * time.Millisecond, MaxAttempts: 1})
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

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

func TestPurgeDeletesDoneJobsAndKeepsFailedOnes(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Concurrency: 2, MaxAttempts: 1, Retention: time.Nanosecond})
	pool := dbtest.Pool(t)
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

	for state, want := range map[queue.State]string{queue.StateDone: "done-1", queue.StateFailed: "failed-1"} {
		listed, err := client.List(t.Context(), state, 0)
		if err != nil {
			t.Fatalf("List(%s) = %v, want no error", state, err)
		}
		if len(listed) != 1 || listed[0].TargetID != want || listed[0].State != state {
			t.Errorf("List(%s) = %+v, want only %s", state, listed, want)
		}
	}

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

func TestPurgeKeepsDoneJobsInsideTheRetentionWindow(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Retention: time.Hour})
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
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

func TestClaimTakesJobsByPriorityThenInOrder(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Concurrency: 2})

	tx, err := dbtest.Pool(t).Begin(t.Context())
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

func TestADelayedJobIsNotClaimedBeforeItIsDue(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1", Delay: 400 * time.Millisecond})

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

func TestTheTraceContextReachesTheWorker(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})
	carrier := map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1", TraceContext: carrier})

	jobs := claim(t, client)
	if len(jobs) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(jobs))
	}
	if got := jobs[0].TraceContext["traceparent"]; got != carrier["traceparent"] {
		t.Errorf("the claimed job's traceparent = %q, want %q", got, carrier["traceparent"])
	}

	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-2"})
	plain := claim(t, client)
	if len(plain) != 1 {
		t.Fatalf("Claim = %d jobs, want 1", len(plain))
	}
	if plain[0].TraceContext == nil || len(plain[0].TraceContext) != 0 {
		t.Errorf("a job enqueued with no trace context has %v, want an empty carrier", plain[0].TraceContext)
	}
}

func TestAnOversizedErrorIsStoredBoundedAndStillValidText(t *testing.T) {
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, MaxAttempts: 1})
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

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

func TestAnErrorHoldingBytesPostgresRefusesIsStillRecorded(t *testing.T) {
	tests := []struct {
		name     string
		cause    error
		readable string
	}{
		{name: "a NUL byte", cause: errors.New("provider said: \x00 truncated"), readable: "provider said:"},
		{name: "invalid UTF-8", cause: errors.New("provider said: \xff\xfe"), readable: "provider said:"},
		{name: "a NUL and invalid UTF-8 together", cause: errors.New("boom\x00\xff"), readable: "boom"},
		{name: "invalid UTF-8 past the length bound", cause: errors.New(strings.Repeat("\xff", 3000))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := newKind(t, false)
			client := newClient(t, queue.Config{Kind: kind, MaxAttempts: 1})
			enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
			jobs := claim(t, client)
			if len(jobs) != 1 {
				t.Fatalf("Claim = %d jobs, want 1", len(jobs))
			}

			state, _, err := client.Fail(t.Context(), jobs[0], tt.cause)
			if err != nil {
				t.Fatalf("Fail = %v, want the attempt recorded: a handler's bytes must not be able to make the write fail", err)
			}
			if state != queue.StateFailed {
				t.Fatalf("Fail = %q, want the job failed on its last attempt", state)
			}

			failed, err := client.List(t.Context(), queue.StateFailed, 0)
			if err != nil || len(failed) != 1 {
				t.Fatalf("List(failed) = %v, %v, want the failed job", failed, err)
			}
			stored := failed[0].LastError
			if stored == "" {
				t.Fatal("nothing was recorded as the cause")
			}
			if !utf8.ValidString(stored) {
				t.Errorf("the stored error is not valid UTF-8: %q", stored)
			}
			if strings.ContainsRune(stored, 0) {
				t.Errorf("a NUL byte survived into the row: %q", stored)
			}
			if tt.readable != "" && !strings.Contains(stored, tt.readable) {
				t.Errorf("the stored error is %q, want the cause %q still readable in it", stored, tt.readable)
			}
			if strings.HasSuffix(stored, "…") {
				t.Errorf("the stored error %q is marked truncated, but it fits the bound once coerced: the bound is counting bytes nobody will read", stored)
			}
		})
	}
}

func TestTheSchemaRefusesRowsTheQueueWouldNeverWrite(t *testing.T) {
	kind := newKind(t, false)
	pool := dbtest.Pool(t)

	const insert = `
INSERT INTO queue_job (kind, target_id, serial_key, state, attempt, started_at, lease_expires_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	now := time.Now()
	noKey := ""
	tests := []struct {
		name       string
		serialKey  *string
		state      string
		attempt    int
		startedAt  *time.Time
		leaseUntil *time.Time
		finishedAt *time.Time
		wantCode   string
		wantName   string
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
			if pgErr.Code != tt.wantCode {
				t.Errorf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, tt.wantCode)
			}
			if name := pgErr.ConstraintName + pgErr.ColumnName; name != tt.wantName {
				t.Errorf("the error names %q, want %q", name, tt.wantName)
			}
		})
	}
}

func expireLeases(t *testing.T, jobs ...queue.Job) {
	t.Helper()
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	if _, err := dbtest.Pool(t).Exec(t.Context(),
		`UPDATE queue_job SET lease_expires_at = now() - interval '1 second' WHERE id = ANY($1) AND state = 'running'`, ids); err != nil {
		t.Fatalf("expiring the leases of %v: %v", ids, err)
	}
}

func reclaim(t *testing.T, client *queue.Client) queue.Reclaimed {
	t.Helper()
	reclaimed, err := client.Reclaim(t.Context())
	if err != nil {
		t.Fatalf("Reclaim = %v, want no error", err)
	}
	return reclaimed
}

func listJobs(t *testing.T, client *queue.Client, state queue.State) []queue.Job {
	t.Helper()
	jobs, err := client.List(t.Context(), state, 0)
	if err != nil {
		t.Fatalf("List(%s) = %v, want no error", state, err)
	}
	return jobs
}

func TestARetryOfATargetThatIsAlreadyPending(t *testing.T) {
	tests := []struct {
		name        string
		maxAttempts int
		want        queue.State
		wantError   string
	}{
		{
			name: "is superseded by the pending job", maxAttempts: 3,
			want: queue.StateDone, wantError: "superseded by a pending job for the same target; the model timed out",
		},
		{
			name: "still fails once its attempts are spent", maxAttempts: 1,
			want: queue.StateFailed, wantError: "the model timed out",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := newKind(t, false)
			client := newClient(t, queue.Config{Kind: kind, MaxAttempts: tt.maxAttempts})
			enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
			running := claim(t, client)
			if len(running) != 1 {
				t.Fatalf("Claim = %d jobs, want 1", len(running))
			}
			pending := enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})

			state, retry, err := client.Fail(t.Context(), running[0], errors.New("the model timed out"))
			if err != nil {
				t.Fatalf("Fail = %v, want no error", err)
			}
			if state != tt.want || retry != (queue.Retry{}) {
				t.Errorf("Fail = %q, %+v, want %q and no retry", state, retry, tt.want)
			}
			finished := listJobs(t, client, tt.want)
			if len(finished) != 1 || finished[0].ID != running[0].ID || finished[0].LastError != tt.wantError ||
				finished[0].FinishedAt.IsZero() {
				t.Errorf("List(%s) = %+v, want job %d finished with LastError %q", tt.want, finished, running[0].ID, tt.wantError)
			}
			if got := listJobs(t, client, queue.StatePending); len(got) != 1 || got[0].ID != pending.ID {
				t.Errorf("List(pending) = %+v, want only job %d, which does the work now", got, pending.ID)
			}
		})
	}
}

func TestReclaimSweepsTheKindWhenOneTargetIsAlreadyPending(t *testing.T) {
	const lease = 50 * time.Millisecond
	kind := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind, Lease: lease, Concurrency: 10})
	for _, target := range []string{"evt-1", "evt-2", "evt-3"} {
		enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: target})
	}
	claimed := claim(t, client)
	if len(claimed) != 3 {
		t.Fatalf("Claim = %d jobs, want 3", len(claimed))
	}
	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-2"})
	expireLeases(t, claimed...)

	if got := reclaim(t, client); got != (queue.Reclaimed{Pending: 2, Superseded: 1}) {
		t.Errorf("Reclaim = %+v, want two jobs requeued and evt-2's superseded", got)
	}
	var targets []string
	for _, job := range listJobs(t, client, queue.StatePending) {
		targets = append(targets, job.TargetID)
	}
	slices.Sort(targets)
	if !slices.Equal(targets, []string{"evt-1", "evt-2", "evt-3"}) {
		t.Errorf("pending targets = %v, want one job for each of the three", targets)
	}
	done := listJobs(t, client, queue.StateDone)
	if len(done) != 1 || done[0].TargetID != "evt-2" ||
		!strings.HasPrefix(done[0].LastError, "superseded by a pending job for the same target; the lease expired") {
		t.Errorf("List(done) = %+v, want evt-2's expired job recorded as superseded", done)
	}
}

func TestReclaimOfTwoExpiredJobsForOneTarget(t *testing.T) {
	const short = 50 * time.Millisecond
	tests := []struct {
		name        string
		newerLease  time.Duration
		failNewer   bool
		wantPending string
		want        queue.Reclaimed
	}{
		{
			name: "requeues the newer and supersedes the older", newerLease: short,
			wantPending: "newer", want: queue.Reclaimed{Pending: 1, Superseded: 1},
		},
		{
			name: "requeues the older when the newer has no attempts left", newerLease: short, failNewer: true,
			wantPending: "older", want: queue.Reclaimed{Pending: 1, Failed: 1},
		},
		{
			name: "requeues the older when the newer is still held", newerLease: time.Minute,
			wantPending: "older", want: queue.Reclaimed{Pending: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := newKind(t, false)
			base := queue.Config{Kind: kind, MaxAttempts: 2, Backoff: time.Millisecond, MaxBackoff: time.Millisecond}
			shortCfg, newerCfg := base, base
			shortCfg.Lease, newerCfg.Lease = short, tt.newerLease
			client := newClient(t, shortCfg)
			newerClient := newClient(t, newerCfg)

			enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
			older := claim(t, client)
			if len(older) != 1 {
				t.Fatalf("Claim = %d jobs, want the older job", len(older))
			}
			enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
			var newer []queue.Job
			if tt.failNewer {
				first := claim(t, newerClient)
				if len(first) != 1 {
					t.Fatalf("Claim = %d jobs, want the newer job", len(first))
				}
				if state, _, err := newerClient.Fail(t.Context(), first[0], errors.New("nope")); err != nil || state != queue.StatePending {
					t.Fatalf("Fail = %q, %v, want the newer job back in pending", state, err)
				}
				waitForClaim(t, newerClient, &newer)
			} else {
				newer = claim(t, newerClient)
			}
			if len(newer) != 1 {
				t.Fatalf("Claim = %d jobs, want the newer job", len(newer))
			}
			expireLeases(t, older...)
			if tt.newerLease == short {
				expireLeases(t, newer...)
			}

			if got := reclaim(t, client); got != tt.want {
				t.Errorf("Reclaim = %+v, want %+v", got, tt.want)
			}
			wantID := newer[0].ID
			if tt.wantPending == "older" {
				wantID = older[0].ID
			}
			if got := listJobs(t, client, queue.StatePending); len(got) != 1 || got[0].ID != wantID {
				t.Errorf("List(pending) = %+v, want the %s job %d", got, tt.wantPending, wantID)
			}
		})
	}
}

func waitForClaim(t *testing.T, client *queue.Client, into *[]queue.Job) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(*into) == 0 && time.Now().Before(deadline); {
		*into = claim(t, client)
	}
}

func TestAFailOrReclaimThatLosesARaceToAnEnqueueIsSuperseded(t *testing.T) {
	const lease = 50 * time.Millisecond
	tests := []struct {
		name string
		op   func(t *testing.T, client *queue.Client, job queue.Job) (queue.State, error)
	}{
		{
			name: "fail",
			op: func(t *testing.T, client *queue.Client, job queue.Job) (queue.State, error) {
				state, _, err := client.Fail(t.Context(), job, errors.New("the model timed out"))
				return state, err
			},
		},
		{
			name: "reclaim",
			op: func(t *testing.T, client *queue.Client, job queue.Job) (queue.State, error) {
				expireLeases(t, job)
				got, err := client.Reclaim(t.Context())
				if got != (queue.Reclaimed{Superseded: 1}) {
					return queue.StatePending, err
				}
				return queue.StateDone, err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind := newKind(t, false)
			pool := dbtest.Pool(t)
			client := newClient(t, queue.Config{Kind: kind, Lease: lease})
			enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-1"})
			running := claim(t, client)
			if len(running) != 1 {
				t.Fatalf("Claim = %d jobs, want 1", len(running))
			}

			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatalf("beginning the enqueue's transaction: %v", err)
			}
			defer tx.Rollback(context.WithoutCancel(t.Context())) //nolint:errcheck // a rollback after commit is a no-op
			var enqueuer int
			if err := tx.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&enqueuer); err != nil {
				t.Fatalf("reading the enqueuer's pid: %v", err)
			}
			enqueue(t, tx, queue.Request{Kind: kind, TargetID: "evt-1"})

			type result struct {
				state queue.State
				err   error
			}
			done := make(chan result, 1)
			go func() {
				state, err := tt.op(t, client, running[0])
				done <- result{state, err}
			}()
			poll.Until(t, "the statement to wait on the enqueue", func() bool {
				var blocked bool
				err := pool.QueryRow(t.Context(),
					"SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))",
					enqueuer).Scan(&blocked)
				return err == nil && blocked
			})
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatalf("committing the enqueue: %v", err)
			}

			got := <-done
			if got.err != nil || got.state != queue.StateDone {
				t.Errorf("%s = %q, %v, want the job superseded and no error", tt.name, got.state, got.err)
			}
			if s := stats(t, client); s.Pending != 1 || s.Running != 0 || s.Done != 1 {
				t.Errorf("stats = %+v, want the enqueued job pending and the old one done", s)
			}
		})
	}
}

func TestUnfinishedIsWhatIsPendingOrRunning(t *testing.T) {
	kind := newKind(t, false)
	other := newKind(t, false)
	client := newClient(t, queue.Config{Kind: kind})
	pool := dbtest.Pool(t)

	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "done"})
	done := claim(t, client)
	if len(done) != 1 {
		t.Fatalf("claimed %v, want one job", done)
	}
	if ok, err := client.Complete(t.Context(), done[0]); err != nil || !ok {
		t.Fatalf("Complete = %v, %v", ok, err)
	}
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "running"})
	if running := claim(t, client); len(running) != 1 {
		t.Fatalf("claimed %v, want one job", running)
	}
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "pending"})
	enqueue(t, pool, queue.Request{Kind: other, TargetID: "elsewhere"})

	got, err := queue.Unfinished(t.Context(), pool, kind, []string{"running", "done", "pending", "elsewhere", "never"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["running"] || !got["pending"] {
		t.Errorf("Unfinished = %v, want running and pending alone", got)
	}
}
