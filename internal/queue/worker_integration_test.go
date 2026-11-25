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

	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/poll"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

func quiet(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx := telemetry.WithLogger(t.Context(), slog.New(slog.DiscardHandler))
	return context.WithCancel(ctx)
}

func run(t *testing.T, cfg queue.Config, handler queue.Handler) func() {
	t.Helper()
	worker, err := queue.NewWorker(dbtest.Pool(t), cfg, handler)
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
	return stop
}

func TestABusyWorkerStillReclaimsExpiredLeases(t *testing.T) {
	kind := newKind(t, false)
	pool := dbtest.Pool(t)

	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-abandoned"})
	abandoning := newClient(t, queue.Config{Kind: kind, Lease: time.Millisecond})
	if got := claim(t, abandoning); len(got) != 1 || got[0].TargetID != "evt-abandoned" {
		t.Fatalf("the staged claim = %v, want the one job, which is then abandoned", got)
	}

	var (
		mu      sync.Mutex
		ran     []string
		feeding = true
		feedErr error
	)
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-feed-0"})
	stop := run(t, queue.Config{
		Kind:                kind,
		Lease:               30 * time.Second,
		MaintenanceInterval: 100 * time.Millisecond,
		PollInterval:        time.Minute,
	}, func(ctx context.Context, job queue.Job) error {
		mu.Lock()
		ran = append(ran, job.TargetID)
		next := "evt-feed-" + strconv.Itoa(len(ran))
		keepFeeding := feeding
		mu.Unlock()

		if keepFeeding {
			if _, err := queue.Enqueue(ctx, pool, queue.Request{Kind: kind, TargetID: next}); err != nil {
				mu.Lock()
				feedErr, feeding = err, false
				mu.Unlock()
			}
		}
		time.Sleep(2 * time.Millisecond)
		return nil
	})

	poll.Until(t, "the abandoned job to be reclaimed and re-run while the worker is busy", func() bool {
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
	if worked < 3 {
		t.Errorf("the worker ran %d jobs before the abandoned one was reclaimed, want a drain loop that stayed fed", worked)
	}
}

func TestAnIdleWorkerReclaimsAnExpiredLease(t *testing.T) {
	kind := newKind(t, false)
	pool := dbtest.Pool(t)

	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-abandoned"})
	abandoning := newClient(t, queue.Config{Kind: kind, Lease: time.Millisecond})
	if got := claim(t, abandoning); len(got) != 1 {
		t.Fatalf("the staged claim = %v, want the one job, which is then abandoned", got)
	}

	var (
		mu  sync.Mutex
		ran []string
	)
	stop := run(t, queue.Config{
		Kind:                kind,
		Lease:               30 * time.Second,
		MaintenanceInterval: 100 * time.Millisecond,
		PollInterval:        time.Minute,
	}, func(_ context.Context, job queue.Job) error {
		mu.Lock()
		ran = append(ran, job.TargetID)
		mu.Unlock()
		return nil
	})

	poll.Until(t, "the idle worker to reclaim the abandoned job and run it", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(ran, "evt-abandoned")
	})
	stop()
}

func TestAWorkerStartsAJobItIsNotifiedOfWithoutWaitingForThePoll(t *testing.T) {
	kind := newKind(t, false)
	handled := make(chan queue.Job, 1)
	stop := run(t, queue.Config{
		Kind: kind, PollInterval: time.Minute, MaintenanceInterval: time.Minute,
	}, func(_ context.Context, job queue.Job) error {
		handled <- job
		return nil
	})

	pool := dbtest.Pool(t)
	poll.Until(t, "the worker to listen on its kind's channel", func() bool {
		var listening bool
		err := pool.QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE query ILIKE 'LISTEN %' AND strpos(query, $1) > 0)`,
			kind.Channel()).Scan(&listening)
		return err == nil && listening
	})

	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "evt-1"})
	select {
	case job := <-handled:
		if job.TargetID != "evt-1" || job.Attempt != 1 {
			t.Errorf("handled %+v, want the job that was enqueued, on attempt 1", job)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never handled the job; nothing woke it and the poll interval is a minute")
	}

	stop()
	client := newClient(t, queue.Config{Kind: kind})
	poll.Until(t, "the job to be recorded as done", func() bool { return stats(t, client).Done == 1 })
}

func TestAWorkerRetriesAFailingHandlerAndThenGivesUp(t *testing.T) {
	kind := newKind(t, false)
	var attempts atomic.Int64
	run(t, queue.Config{
		Kind: kind, MaxAttempts: 3, Backoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
		PollInterval: 20 * time.Millisecond, MaintenanceInterval: time.Minute,
	}, func(context.Context, queue.Job) error {
		attempts.Add(1)
		return errors.New("the model refused")
	})

	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	client := newClient(t, queue.Config{Kind: kind})
	poll.Until(t, "the job to run out of attempts", func() bool { return stats(t, client).Failed == 1 })

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

func TestAWorkerShuttingDownCancelsAndRecordsTheJobItWasRunning(t *testing.T) {
	kind := newKind(t, false)
	started := make(chan struct{})
	var once sync.Once
	stop := run(t, queue.Config{
		Kind: kind, PollInterval: 20 * time.Millisecond, MaintenanceInterval: time.Minute,
	}, func(ctx context.Context, _ queue.Job) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	})

	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker never started the job")
	}

	stop()
	client := newClient(t, queue.Config{Kind: kind})
	got := stats(t, client)
	if got.Running != 0 || got.Pending != 1 {
		t.Errorf("stats after a shutdown = %+v, want the job back in the queue rather than left running", got)
	}
}

func TestAWorkerStopsRunningAJobWhoseLeaseWasReclaimed(t *testing.T) {
	kind := newKind(t, false)
	cancelled := make(chan struct{})
	var once sync.Once
	var running atomic.Bool
	run(t, queue.Config{
		Kind: kind, Lease: 20 * time.Millisecond, MaxAttempts: 10,
		Backoff: time.Second, MaxBackoff: time.Second,
		PollInterval: 20 * time.Millisecond, MaintenanceInterval: time.Minute,
	}, func(ctx context.Context, _ queue.Job) error {
		if !running.CompareAndSwap(false, true) {
			return nil
		}
		<-ctx.Done()
		once.Do(func() { close(cancelled) })
		return ctx.Err()
	})

	enqueue(t, dbtest.Pool(t), queue.Request{Kind: kind, TargetID: "evt-1"})
	poll.Until(t, "the worker to start the job", func() bool { return running.Load() })

	reclaimer := newClient(t, queue.Config{Kind: kind, Lease: 20 * time.Millisecond, MaxAttempts: 10})
	poll.Until(t, "the expired lease to be reclaimed", func() bool {
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
