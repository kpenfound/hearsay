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

type held struct {
	job queue.Job
	err error
}

func holdAsync(ctx context.Context, client *queue.Client, key, target string) <-chan held {
	out := make(chan held, 1)
	go func() {
		job, err := client.Hold(ctx, key, target)
		out <- held{job, err}
	}()
	return out
}

// stillWaiting fails the test if the hold returned within a few polls.
func stillWaiting(t *testing.T, h <-chan held, why string) {
	t.Helper()
	select {
	case got := <-h:
		t.Fatalf("Hold returned %v, %v while %s", got.job, got.err, why)
	case <-time.After(4 * queue.HoldPoll):
	}
}

func receive(t *testing.T, h <-chan held) queue.Job {
	t.Helper()
	select {
	case got := <-h:
		if got.err != nil {
			t.Fatalf("Hold = %v, want the key", got.err)
		}
		return got.job
	case <-time.After(10 * time.Second):
		t.Fatal("Hold did not return once the key was free")
	}
	return queue.Job{}
}

// A hold waits for the job running under its key and for nothing enqueued
// after it: from the moment it is taken no claim takes the key, while other
// keys run on.
func TestAHoldWaitsForTheRunningJobAndBlocksItsKey(t *testing.T) {
	kind := newKind(t, true)
	client := newClient(t, queue.Config{Kind: kind, Lease: 30 * time.Second})
	pool := newPool(t)
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "a1", SerialKey: "a"})
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "a2", SerialKey: "a"})
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "b1", SerialKey: "b"})

	running := claim(t, client)
	if len(running) != 1 || running[0].TargetID != "a1" {
		t.Fatalf("the first claim = %v, want a1", running)
	}
	h := holdAsync(t.Context(), client, "a", "hold")
	stillWaiting(t, h, "a1 is running on its key")

	if other := claim(t, client); len(other) != 1 || other[0].TargetID != "b1" {
		t.Fatalf("a claim while key a is held = %v, want b1: other keys are not held", other)
	}
	if ok, err := client.Complete(t.Context(), running[0]); err != nil || !ok {
		t.Fatalf("Complete(a1) = %v, %v", ok, err)
	}
	job := receive(t, h)
	if job.State != queue.StateRunning || job.SerialKey != "a" || job.TargetID != "hold" {
		t.Fatalf("the hold = %+v, want a running job for its target on key a", job)
	}
	if next := claim(t, client); len(next) != 0 {
		t.Fatalf("a claim while the hold stands = %v, want nothing: a2 waits for it", next)
	}

	// The hold ends with the transaction that did the work.
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := client.CompleteIn(t.Context(), tx, job); err != nil || !ok {
		t.Fatalf("CompleteIn = %v, %v", ok, err)
	}
	if next := claim(t, client); len(next) != 0 {
		t.Fatalf("a claim before the hold's transaction commits = %v, want nothing", next)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if next := claim(t, client); len(next) != 1 || next[0].TargetID != "a2" {
		t.Fatalf("a claim after the hold = %v, want a2", next)
	}
}

// Two holds on one key take turns, in the order they were taken.
func TestTwoHoldsOnOneKeyTakeTurns(t *testing.T) {
	kind := newKind(t, true)
	client := newClient(t, queue.Config{Kind: kind, Lease: 30 * time.Second})
	first, err := client.Hold(t.Context(), "a", "first")
	if err != nil {
		t.Fatalf("Hold(first) = %v", err)
	}
	second := holdAsync(t.Context(), client, "a", "second")
	stillWaiting(t, second, "the first hold stands")
	if ok, err := client.Complete(t.Context(), first); err != nil || !ok {
		t.Fatalf("Complete(first) = %v, %v", ok, err)
	}
	if job := receive(t, second); job.TargetID != "second" {
		t.Fatalf("the second hold = %+v", job)
	}
}

// A caller that gives up waiting lets the key go rather than blocking it for a
// lease.
func TestAHoldGivenUpLetsTheKeyGo(t *testing.T) {
	kind := newKind(t, true)
	client := newClient(t, queue.Config{Kind: kind, Lease: 30 * time.Second})
	pool := newPool(t)
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "a1", SerialKey: "a"})
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "a2", SerialKey: "a"})
	running := claim(t, client)

	ctx, cancel := context.WithTimeout(t.Context(), 2*queue.HoldPoll)
	defer cancel()
	if _, err := client.Hold(ctx, "a", "hold"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Hold(a busy key, a short deadline) = %v, want the deadline", err)
	}
	if ok, err := client.Complete(t.Context(), running[0]); err != nil || !ok {
		t.Fatalf("Complete(a1) = %v, %v", ok, err)
	}
	if next := claim(t, client); len(next) != 1 || next[0].TargetID != "a2" {
		t.Fatalf("a claim after the hold gave up = %v, want a2", next)
	}
}

// A hold whose lease lapsed has lost the key: completing it in the work's
// transaction says so, which is the caller's signal to roll the work back, and
// the reclaimed row is a job like any other.
func TestAHoldWhoseLeaseLapsedCannotComplete(t *testing.T) {
	const lease = 50 * time.Millisecond
	kind := newKind(t, true)
	client := newClient(t, queue.Config{Kind: kind, Lease: lease})
	pool := newPool(t)
	job, err := client.Hold(t.Context(), "a", "hold")
	if err != nil {
		t.Fatalf("Hold = %v", err)
	}
	expireLeases(lease)
	if r := reclaim(t, client); r.Pending != 1 {
		t.Fatalf("Reclaim = %+v, want the hold back in the queue", r)
	}
	err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		ok, err := client.CompleteIn(t.Context(), tx, job)
		if err != nil || ok {
			t.Errorf("CompleteIn(a reclaimed hold) = %v, %v, want false", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if next := claim(t, client); len(next) != 1 || next[0].TargetID != "hold" {
		t.Fatalf("a claim after the lapse = %v, want the hold's job for the worker to finish", next)
	}
}

func TestOnlyASerializedKindIsHeld(t *testing.T) {
	client := newClient(t, queue.Config{Kind: newKind(t, false)})
	if _, err := client.Hold(t.Context(), "a", "hold"); !errors.Is(err, queue.ErrInvalidJob) {
		t.Errorf("Hold(an unserialized kind) = %v, want ErrInvalidJob", err)
	}
	serialized := newClient(t, queue.Config{Kind: newKind(t, true)})
	if _, err := serialized.Hold(t.Context(), "", "hold"); !errors.Is(err, queue.ErrInvalidJob) {
		t.Errorf("Hold(no key) = %v, want ErrInvalidJob", err)
	}
}

// A job whose lease expired is one the queue no longer believes is running,
// and a hold does not wait for a reclaimer to say so: the kind's worker may be
// the thing that is down.
func TestAHoldDoesNotWaitOnAnExpiredLease(t *testing.T) {
	const lease = 50 * time.Millisecond
	kind := newKind(t, true)
	dead := newClient(t, queue.Config{Kind: kind, Lease: lease})
	client := newClient(t, queue.Config{Kind: kind, Lease: 30 * time.Second})
	pool := newPool(t)
	enqueue(t, pool, queue.Request{Kind: kind, TargetID: "a1", SerialKey: "a"})
	if running := claim(t, dead); len(running) != 1 {
		t.Fatalf("the claim = %v, want a1", running)
	}
	expireLeases(lease)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := client.Hold(ctx, "a", "hold"); err != nil {
		t.Fatalf("Hold(behind an expired lease) = %v, want the key", err)
	}
}
