//go:build integration

package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/queue"
)

func listenForRelease(t *testing.T, kind queue.Kind) *pgx.Conn {
	t.Helper()
	pool := newPool(t)
	conn, err := pgx.ConnectConfig(t.Context(), pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err = conn.Exec(t.Context(), "LISTEN "+pgx.Identifier{kind.Channel()}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestHoldCompletionNotifiesOnlyOnCommitAndWakesWorker(t *testing.T) {
	pool := newPool(t)
	kind := newKind(t, true)
	cfg := queue.Config{Kind: kind, Lease: 30 * time.Second, PollInterval: 20 * time.Second}
	client, err := queue.New(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	held, err := client.Hold(t.Context(), "scope", "hold")
	if err != nil {
		t.Fatal(err)
	}
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "waiting", SerialKey: "scope"})
	observer := listenForRelease(t, kind)
	claimed := make(chan time.Time, 1)
	_, _ = run(t, cfg, func(_ context.Context, j queue.Job) error {
		if j.TargetID == "waiting" {
			claimed <- time.Now()
		}
		return nil
	})
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := client.CompleteIn(t.Context(), tx, held); err != nil || !changed {
		t.Fatalf("CompleteIn=%v %v", changed, err)
	}
	// Transactional NOTIFY must be invisible while the completion is uncommitted.
	wait, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = observer.WaitForNotification(wait)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncommitted notification: %v", err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-claimed:
		t.Fatal("worker acquired a rolled-back hold")
	default:
	}
	if jobs := claim(t, client); len(jobs) != 0 {
		t.Fatal("rollback released the key")
	}
	wait, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = observer.WaitForNotification(wait)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rollback notification: %v", err)
	}
	released := time.Now()
	err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		changed, err := client.CompleteIn(t.Context(), tx, held)
		if err == nil && !changed {
			t.Error("hold not completed")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	wait, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = observer.WaitForNotification(wait); err != nil {
		t.Fatalf("release not announced: %v", err)
	}
	select {
	case acquired := <-claimed:
		if acquired.Sub(released) > time.Second {
			t.Fatalf("claim took %s with a 20s poll floor", acquired.Sub(released))
		}
	case <-wait.Done():
		t.Fatal("worker waited for the polling floor after a committed hold release")
	}
}

func TestFailureAndReclaimAnnounceReleasedKeys(t *testing.T) {
	for _, mode := range []string{"failure", "reclaim"} {
		t.Run(mode, func(t *testing.T) {
			pool := newPool(t)
			kind := newKind(t, true)
			lease := 30 * time.Second
			if mode == "reclaim" {
				lease = 20 * time.Millisecond
			}
			client, err := queue.New(pool, queue.Config{Kind: kind, Lease: lease, MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			enqueue(t, pool, queue.Request{Kind: kind, TargetID: "running", SerialKey: "scope"})
			jobs := claim(t, client)
			if len(jobs) != 1 {
				t.Fatal("not claimed")
			}
			observer := listenForRelease(t, kind)
			if mode == "failure" {
				_, _, err = client.Fail(t.Context(), jobs[0], errors.New("fixture"))
			} else {
				time.Sleep(2 * lease)
				_, err = client.Reclaim(t.Context())
			}
			if err != nil {
				t.Fatal(err)
			}
			wait, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err = observer.WaitForNotification(wait); err != nil {
				t.Fatalf("%s failed to announce release: %v", mode, err)
			}
		})
	}
}
