//go:build integration

package queue_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// quiet is a worker's context with a logger that goes nowhere: the loop logs
// what it does, and a test that passes has nothing to say.
func quiet(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx := telemetry.WithLogger(t.Context(), slog.New(slog.DiscardHandler))
	return context.WithCancel(ctx)
}

// run starts a worker and returns a function that stops it and reports what
// Run returned. A worker asked to stop has not failed, so that is nil.
func run(t *testing.T, cfg queue.Config, handler queue.Handler) (*queue.Worker, func()) {
	t.Helper()
	worker, err := queue.NewWorker(newPool(t), cfg, handler)
	if err != nil {
		t.Fatalf("NewWorker = %v, want no error", err)
	}
	ctx, cancel := quiet(t)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run = %v, want nil: a worker asked to stop has not failed", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	}
	t.Cleanup(stop)
	return worker, stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A worker with a backlog still does its maintenance. The drain loop is where
// a busy worker lives — ADR-0007's distiller backfill keeps it there for as
// long as the backfill lasts — so if the maintenance tick were only read by
// the outer select, a kind's expired leases would go unreclaimed for exactly
// as long as there is most to reclaim, and every extra worker would make that
// worse rather than better, because they would all be draining.
//
// Staged rather than raced: a job is claimed with a lease measured in
// milliseconds and then abandoned, which is the row a worker that died
// mid-job leaves behind, and the work that keeps the loop fed enqueues its own
// successor so that a claim never comes back empty.
func TestABusyWorkerStillReclaimsExpiredLeases(t *testing.T) {
	kind := newKind(t, false)
	pool := newPool(t)

	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-abandoned"})
	abandoning := newClient(t, queue.Config{Kind: kind, Lease: time.Millisecond})
	if got := claim(t, abandoning); len(got) != 1 || got[0].TargetID != "evt-abandoned" {
		t.Fatalf("the staged claim = %v, want the one job, which is then abandoned", got)
	}
	// Nothing completes it, so it is `running` with a lease that is already
	// gone. Only a reclaim can make it claimable again — which is the point:
	// the worker below cannot reach it any other way.

	var (
		mu      sync.Mutex
		ran     []string
		feeding = true
		feedErr error
	)
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-feed-0"})
	_, stop := run(t, queue.Config{
		Kind:                kind,
		Lease:               30 * time.Second,
		MaintenanceInterval: 100 * time.Millisecond,
		// Long, so that nothing here is explained by a poll: the drain loop
		// never sleeps while there is work anyway.
		PollInterval: time.Minute,
	}, func(ctx context.Context, job queue.Job) error {
		mu.Lock()
		ran = append(ran, job.TargetID)
		next := "evt-feed-" + strconv.Itoa(len(ran))
		keepFeeding := feeding
		mu.Unlock()

		if keepFeeding {
			// Enqueued before the handler returns, so there is always
			// something pending when the loop claims again.
			if _, err := queue.Enqueue(ctx, pool, queue.Request{Kind: kind, TargetID: next}); err != nil {
				mu.Lock()
				feedErr, feeding = err, false
				mu.Unlock()
			}
		}
		time.Sleep(2 * time.Millisecond)
		return nil
	})

	waitFor(t, "the abandoned job to be reclaimed and re-run while the worker is busy", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(ran, "evt-abandoned")
	})

	mu.Lock()
	feeding = false
	worked, err := len(ran), feedErr
	mu.Unlock()
	stop()

	if err != nil {
		t.Fatalf("keeping the worker fed failed: %v", err)
	}
	// The reclaim has to have happened while the loop had work, or the test
	// proved nothing: with only the abandoned job and its feed, a worker that
	// had gone idle would show barely any runs.
	if worked < 3 {
		t.Errorf("the worker ran %d jobs before the abandoned one was reclaimed, want a drain loop that stayed fed", worked)
	}
}

// LISTEN/NOTIFY is what makes a job start now rather than at the next poll.
// The polling floor here is a minute, so a job that is handled within seconds
// was handled because the enqueue announced it.
func TestAWorkerStartsAJobItIsNotifiedOfWithoutWaitingForThePoll(t *testing.T) {
	kind := newKind(t, false)
	handled := make(chan queue.Job, 1)
	worker, stop := run(t, queue.Config{
		Kind: kind, PollInterval: time.Minute, MaintenanceInterval: time.Minute,
	}, func(_ context.Context, job queue.Job) error {
		handled <- job
		return nil
	})

	// Let the worker settle into its wait: a job enqueued before it starts
	// would be found by the first claim, which would prove nothing about the
	// notification.
	time.Sleep(500 * time.Millisecond)

	enqueued := time.Now()
	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	select {
	case job := <-handled:
		if latency := time.Since(enqueued); latency > 30*time.Second {
			t.Errorf("the job was handled after %s, want the notification to have woken the worker", latency)
		}
		if job.TargetID != "evt-1" || job.Attempt != 1 {
			t.Errorf("handled %+v, want the job that was enqueued, on attempt 1", job)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never handled the job; nothing woke it and the poll interval is a minute")
	}

	stop()
	client := worker.Client()
	waitFor(t, "the job to be recorded as done", func() bool { return stats(t, client).Done == 1 })
}

// A handler that keeps failing is retried up to the limit and then gives up,
// through the loop rather than through the client's own Fail.
func TestAWorkerRetriesAFailingHandlerAndThenGivesUp(t *testing.T) {
	kind := newKind(t, false)
	var attempts atomic.Int64
	worker, _ := run(t, queue.Config{
		Kind: kind, MaxAttempts: 3, Backoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
		PollInterval: 20 * time.Millisecond, MaintenanceInterval: time.Minute,
	}, func(context.Context, queue.Job) error {
		attempts.Add(1)
		return errors.New("the model refused")
	})

	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	client := worker.Client()
	waitFor(t, "the job to run out of attempts", func() bool { return stats(t, client).Failed == 1 })

	if got := attempts.Load(); got != 3 {
		t.Errorf("the handler ran %d times, want the 3 attempts it was given", got)
	}
	failed, err := client.List(t.Context(), queue.StateFailed, 0)
	if err != nil {
		t.Fatalf("List(failed) = %v, want no error", err)
	}
	if len(failed) != 1 || failed[0].LastError != "the model refused" {
		t.Errorf("List(failed) = %+v, want the job with the handler's error on it", failed)
	}
}

// A worker that is shutting down cancels the handler and still records what
// happened, rather than leaving a running row for the reclaimer to work out.
func TestAWorkerShuttingDownCancelsAndRecordsTheJobItWasRunning(t *testing.T) {
	kind := newKind(t, false)
	started := make(chan struct{})
	var once sync.Once
	worker, stop := run(t, queue.Config{
		Kind: kind, PollInterval: 20 * time.Millisecond, MaintenanceInterval: time.Minute,
	}, func(ctx context.Context, _ queue.Job) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	})

	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker never started the job")
	}

	stop()
	client := worker.Client()
	got := stats(t, client)
	if got.Running != 0 || got.Pending != 1 {
		t.Errorf("stats after a shutdown = %+v, want the job back in the queue rather than left running", got)
	}
}

// A worker whose lease has been taken away is told so by its own heartbeat,
// and stops working on a job something else is now running. The lease here is
// shorter than the heartbeat floor, which is how the test gets an expiry
// without waiting a minute for one.
func TestAWorkerStopsRunningAJobWhoseLeaseWasReclaimed(t *testing.T) {
	kind := newKind(t, false)
	cancelled := make(chan struct{})
	var once sync.Once
	var running atomic.Bool
	_, _ = run(t, queue.Config{
		Kind: kind, Lease: 20 * time.Millisecond, MaxAttempts: 10,
		Backoff: time.Second, MaxBackoff: time.Second,
		PollInterval: 20 * time.Millisecond, MaintenanceInterval: time.Minute,
	}, func(ctx context.Context, _ queue.Job) error {
		if !running.CompareAndSwap(false, true) {
			// A later run of the same job: it is somebody else's now, and
			// blocking again would only hide the first one's cancellation.
			return nil
		}
		<-ctx.Done()
		once.Do(func() { close(cancelled) })
		return ctx.Err()
	})

	enqueue(t, newPool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	waitFor(t, "the worker to start the job", func() bool { return running.Load() })

	// Something else takes the expired lease. The worker's next heartbeat is
	// what tells it, and cancelling the run is what it does about it.
	reclaimer := newClient(t, queue.Config{Kind: kind, Lease: 20 * time.Millisecond, MaxAttempts: 10})
	waitFor(t, "the expired lease to be reclaimed", func() bool {
		reclaimed, err := reclaimer.Reclaim(t.Context())
		if err != nil {
			t.Fatalf("Reclaim = %v, want no error", err)
		}
		return reclaimed.Total() > 0
	})

	select {
	case <-cancelled:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker kept running a job whose lease had been reclaimed")
	}
}
