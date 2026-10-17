//go:build integration

package distiller_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func distillQueue(t *testing.T, pool *pgxpool.Pool) *queue.Client {
	t.Helper()
	c, err := queue.New(pool, queue.Config{Kind: distiller.JobKind()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func onePending(t *testing.T, c *queue.Client) queue.Job {
	t.Helper()
	jobs, err := c.List(t.Context(), queue.StatePending, 100)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("List(pending) = %+v, %v, want one job", jobs, err)
	}
	return jobs[0]
}

// Read until these events are visible: L0's safe feed horizon can be held
// behind transactions in other databases on the same test cluster.
func pumpEvents(t *testing.T, p *distiller.Pump, count int) distiller.Progress {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	total := 0
	var last distiller.Progress
	for total < count {
		got, err := p.Once(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		total += got.Events
		if got.Events > 0 {
			last = got
		}
		if time.Now().After(deadline) {
			t.Fatalf("Once read %d events, want %d", total, count)
		}
		if total < count {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return last
}

// Wait without pumping so the burst is collapsed in one enqueue transaction.
func waitForPumpBatch(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	from, err := l0.NewCursors(pool).Load(t.Context(), distiller.Consumer)
	if err != nil {
		t.Fatal(err)
	}
	events := l0.New(pool)
	deadline := time.Now().Add(30 * time.Second)
	for {
		changes, err := events.Changes(t.Context(), from, l0.Filter{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Changes returned %d events, want %d in one batch", len(changes), count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Revisions keep the recorded commit's current text: ten L0 changes still
// make exactly the request in the existing recorded fixture.
func commitRevision(src string, i int) connector.Event {
	return revised(fixtureEvents(src)[6], fmt.Sprintf("burst-%03d", i), at(7))
}

func TestThePumpDebouncesBurstsWithinAndAcrossReads(t *testing.T) {
	for _, batch := range []int{1, 100} {
		t.Run(fmt.Sprintf("batch-%d", batch), func(t *testing.T) {
			pool := newScratchPool(t)
			src := newSource(t)
			opts := distiller.PumpOptions{Quiet: 2 * time.Second, MaxDelay: 5 * time.Second, Batch: batch}
			p := distiller.NewPump(pool, opts)
			c := distillQueue(t, pool)
			for i := 0; i < 10; i++ {
				ingest(t, pool, []connector.Event{commitRevision(src, i)})
			}
			var progress distiller.Progress
			if batch == 100 {
				waitForPumpBatch(t, pool, 10)
				var err error
				progress, err = p.Once(t.Context())
				if err != nil || progress.Events != 10 {
					t.Fatalf("Once = %+v, %v, want 10 events in one read", progress, err)
				}
			} else {
				progress = pumpEvents(t, p, 10)
			}
			job := onePending(t, c)
			if job.TargetID != l1.DocID(src, commit) || job.RunAfter.Sub(job.EnqueuedAt) < opts.Quiet || job.RunAfter.After(job.EnqueuedAt.Add(opts.MaxDelay)) {
				t.Fatalf("debounced schedule = %+v, want quiet %s capped at %s", job, opts.Quiet, opts.MaxDelay)
			}
			if batch == 100 && job.RunAfter.Sub(job.EnqueuedAt) != opts.Quiet {
				t.Fatalf("initial run_after - enqueued_at = %s, want configured quiet %s", job.RunAfter.Sub(job.EnqueuedAt), opts.Quiet)
			}
			saved, err := l0.NewCursors(pool).Load(t.Context(), distiller.Consumer)
			if err != nil || saved != progress.Cursor {
				t.Fatalf("saved cursor = %v, %v, want %v", saved, err, progress.Cursor)
			}
			if jobs, err := c.Claim(t.Context()); err != nil || len(jobs) != 0 {
				t.Fatalf("Claim before quiet = %v, %v", jobs, err)
			}

			// A normal queue poll claims the single job once its quiet period ends.
			registry, err := llm.NewFake(testRepo(src).LLM, loadFixtures(t))
			if err != nil {
				t.Fatal(err)
			}
			d, err := distiller.New(pool, registry, testConfig(src))
			if err != nil {
				t.Fatal(err)
			}
			jobs := claimWhenDue(t, c)
			if len(jobs) != 1 {
				t.Fatalf("Claim = %v, want one execution", jobs)
			}
			if err := d.Handle(t.Context(), jobs[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Complete(t.Context(), jobs[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := l1.New(pool).Get(t.Context(), job.TargetID); err != nil {
				t.Fatal(err)
			}
			calls := 0
			for _, u := range registry.Usage() {
				if u.Tier == llm.TierDistill {
					calls += u.Calls
				}
			}
			done, err := c.List(t.Context(), queue.StateDone, 100)
			if err != nil || len(done) != 1 || calls != 1 {
				t.Fatalf("executions=%d, model calls=%d, error=%v; want one each", len(done), calls, err)
			}
			if jobs, err := c.Claim(t.Context()); err != nil || len(jobs) != 0 {
				t.Fatalf("second Claim = %v, %v, want no more work", jobs, err)
			}
		})
	}
}

func claimWhenDue(t *testing.T, c *queue.Client) []queue.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		jobs, err := c.Claim(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) > 0 {
			return jobs
		}
		if time.Now().After(deadline) {
			t.Fatal("job never became claimable")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestASteadyStreamBecomesDueAtTheMaximumAndDistils(t *testing.T) {
	pool := newScratchPool(t)
	src := newSource(t)
	opts := distiller.PumpOptions{Quiet: 750 * time.Millisecond, MaxDelay: 1500 * time.Millisecond}
	p := distiller.NewPump(pool, opts)
	c := distillQueue(t, pool)
	ingest(t, pool, []connector.Event{commitRevision(src, 0)})
	pumpEvents(t, p, 1)
	first := onePending(t, c)
	capAt := first.EnqueuedAt.Add(opts.MaxDelay)
	capped := false
	for i := 1; ; i++ {
		time.Sleep(100 * time.Millisecond)
		ingest(t, pool, []connector.Event{commitRevision(src, i)})
		pumpEvents(t, p, 1)
		job := onePending(t, c)
		if job.ID != first.ID || !job.EnqueuedAt.Equal(first.EnqueuedAt) || job.RunAfter.After(capAt) {
			t.Fatalf("stream moved first enqueue or passed cap: first=%+v, now=%+v", first, job)
		}
		if job.RunAfter.Equal(capAt) {
			capped = true
		}
		var now time.Time
		if err := pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		if !now.Before(capAt) {
			break
		}
	}
	if !capped {
		t.Fatal("stream never reached maximum-delay cap")
	}
	jobs, err := c.Claim(t.Context())
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim at maximum = %v, %v, want one job", jobs, err)
	}
	d := newDistiller(t, pool, src)
	if err := d.Handle(t.Context(), jobs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(t.Context(), jobs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := l1.New(pool).Get(t.Context(), first.TargetID); err != nil {
		t.Fatal(err)
	}
}

func TestDebouncedJobsRollBackWhenThePumpCannotSaveItsCursor(t *testing.T) {
	pool := newScratchPool(t)
	src := newSource(t)
	ingest(t, pool, []connector.Event{commitRevision(src, 0)})
	if _, err := pool.Exec(t.Context(), `CREATE FUNCTION refuse_cursor() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'cursor refused'; END $$;
 CREATE TRIGGER refuse_cursor BEFORE INSERT ON l0_feed_cursors FOR EACH ROW EXECUTE FUNCTION refuse_cursor()`); err != nil {
		t.Fatal(err)
	}
	p := distiller.NewPump(pool, distiller.PumpOptions{Quiet: time.Minute, MaxDelay: 5 * time.Minute})
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := p.Once(t.Context())
		if err != nil {
			if !strings.Contains(err.Error(), "cursor refused") {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("feed never reached cursor save")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c := distillQueue(t, pool)
	if jobs, err := c.List(t.Context(), queue.StatePending, 100); err != nil || len(jobs) != 0 {
		t.Fatalf("after cursor rollback jobs = %v, %v", jobs, err)
	}
	if cursor, err := l0.NewCursors(pool).Load(t.Context(), distiller.Consumer); err != nil || cursor != (l0.Cursor{}) {
		t.Fatalf("cursor after rollback = %v, %v", cursor, err)
	}
	if _, err := pool.Exec(t.Context(), `DROP TRIGGER refuse_cursor ON l0_feed_cursors`); err != nil {
		t.Fatal(err)
	}
	pumpEvents(t, p, 1)
	onePending(t, c)
}

func TestDeletionExpeditesAPendingRebuildDespiteAFurtherPumpRead(t *testing.T) {
	pool := newScratchPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src)[:3])
	d := newDistiller(t, pool, src)
	doc := l1.DocID(src, repo+"#12")
	if _, err := d.Distill(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	p := distiller.NewPump(pool, distiller.PumpOptions{Quiet: time.Minute, MaxDelay: 5 * time.Minute})
	pumpEvents(t, p, 3)
	c := distillQueue(t, pool)
	first := onePending(t, c)
	if jobs, err := c.Claim(t.Context()); err != nil || len(jobs) != 0 {
		t.Fatalf("Claim before deletion = %v, %v", jobs, err)
	}
	if _, err := deletion.Apply(t.Context(), pool, testRepo(src), deletion.Selector{Event: connector.EventID(src, retractedComment)}, "remove pasted content", "kyle"); err != nil {
		t.Fatal(err)
	}
	// A further revision of the root arrives after Apply commits, before claim.
	ingest(t, pool, []connector.Event{revised(fixtureEvents(src)[0], "after-deletion", at(0))})
	pumpEvents(t, p, 2) // deletion event and the root revision
	pending := onePending(t, c)
	if pending.ID != first.ID {
		t.Fatalf("deletion replaced job %d with %d", first.ID, pending.ID)
	}
	jobs, err := c.Claim(t.Context())
	if err != nil || len(jobs) != 1 || jobs[0].ID != first.ID {
		t.Fatalf("Claim after deletion and pump = %v, %v, want original job due", jobs, err)
	}
	if err := d.Handle(t.Context(), jobs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(t.Context(), jobs[0]); err != nil {
		t.Fatal(err)
	}
	got, err := l1.New(pool).Get(t.Context(), doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range got.L0Refs {
		if id == connector.EventID(src, retractedComment) {
			t.Fatal("rebuilt document still cites deleted event")
		}
	}
}
