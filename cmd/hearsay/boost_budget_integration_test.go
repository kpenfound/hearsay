//go:build integration

package main

import (
	"encoding/json"
	"testing"
	"time"

	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func boostJobTimes(t *testing.T, w *boostWorld, target string) (time.Time, time.Time) {
	t.Helper()
	var start, end time.Time
	boostWait(t, func() bool {
		err := w.pool.QueryRow(t.Context(), `SELECT started_at,finished_at FROM queue_job WHERE kind='assert' AND target_id=$1 AND state='done' ORDER BY id DESC LIMIT 1`, target).Scan(&start, &end)
		return err == nil
	})
	return start, end
}

func TestBoostJobBudgetReleasesScopeToQueuedWriteAndHuman(t *testing.T) {
	baseline := newBoostWorld(t, "off", true)
	baseline.ingest(t, 0)
	baseline.awaitAsserted(t, 0)
	baseline.ingest(t, 2)
	baseline.awaitAsserted(t, 2)
	baseline.ingest(t, 1)
	baseline.awaitAsserted(t, 1)
	bs, be := boostJobTimes(t, baseline, baseline.docs[1].ID)
	baselineTime := be.Sub(bs)
	w := newBoostWorld(t, "budget", true)
	w.ingest(t, 0)
	w.awaitAsserted(t, 0)
	w.ingest(t, 2)
	w.awaitAsserted(t, 2)
	var topic string
	if err := w.pool.QueryRow(t.Context(), `SELECT id FROM l2_topics ORDER BY id LIMIT 1`).Scan(&topic); err != nil {
		t.Fatal(err)
	}
	// Record the real connector command ahead of the input snapshot, with its
	// job scheduled for later. Making it due below tests scope serialization
	// without also changing the judgment's conversation membership mid-call.
	command := w.events[1]
	command.ID = ""
	command.NativeID = "acme/api#2:comment:99"
	command.Kind = connector.KindCommand
	command.Payload.Artifact = command.NativeID
	command.Payload.Parent, command.Payload.Thread = "acme/api#2", "acme/api#2"
	command.Payload.Text = "/hearsay demote"
	native, err := json.Marshal(github.Command{Comment: 99, Repo: "acme/api", Issue: 2, Name: "demote"})
	if err != nil {
		t.Fatal(err)
	}
	command.Payload.Native = native
	humanTarget := l2.GestureTarget + connector.EventID(command.Source, command.NativeID)
	if err = pgx.BeginFunc(t.Context(), w.pool, func(tx pgx.Tx) error {
		if _, err := l0.New(tx).Append(t.Context(), command); err != nil {
			return err
		}
		_, err := queue.Enqueue(t.Context(), tx, queue.Request{Kind: l2.AssertKind(), TargetID: humanTarget, SerialKey: "team", Delay: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.ingest(t, 1)
	// Wait for the first stance provider attempt, proving the job holds the
	// serial key before later work is queued. No sleep guesses or lease expiry.
	boostWait(t, func() bool { return w.fake.Calls() >= 4 })
	var heldStart time.Time
	if err := w.pool.QueryRow(t.Context(), `SELECT started_at FROM queue_job WHERE kind='assert' AND target_id=$1 AND state='running'`, w.docs[1].ID).Scan(&heldStart); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"topic": topic, "position": "Later agent write.", "evidence": []string{w.docs[0].ID}})
	raw, err := w.calls.Call(t.Context(), api.Caller{Principal: "pat", Agent: "bot"}, "assert", args)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw
	// Make the already recorded human command due while this job holds the key.
	if _, err = queue.Expedite(t.Context(), w.pool, l2.AssertKind(), humanTarget); err != nil {
		t.Fatal(err)
	}
	// Both later jobs must be waiting while the optional provider is slow.
	boostWait(t, func() bool {
		var n int
		err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job WHERE kind='assert' AND serial_key='team' AND target_id<>$1 AND state IN ('pending','running')`, w.docs[1].ID).Scan(&n)
		return err == nil && n >= 2
	})
	w.awaitAsserted(t, 1)
	start, end := boostJobTimes(t, w, w.docs[1].ID)
	humanStart, _ := boostJobTimes(t, w, humanTarget)
	// HTTP assert returns assertion_id. Resolve the queued target from the
	// real queue to measure the worker's claim time (not its lease deadline).
	var target string
	if err = w.pool.QueryRow(t.Context(), `SELECT target_id FROM queue_job WHERE kind='assert' AND target_id LIKE 'evt:hearsay:%' ORDER BY id DESC LIMIT 1`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	laterStart, _ := boostJobTimes(t, w, target)
	budget := w.cfg.Repo.Boost.AssertionJobBudget
	w.provider.mu.Lock()
	timings := append([]judgmentTiming(nil), w.provider.assert...)
	w.provider.mu.Unlock()
	if len(timings) != 6 {
		t.Fatalf("provider attempts=%d", len(timings))
	}
	for _, span := range timings {
		if !span.deadline.Equal(timings[0].deadline) {
			t.Fatal("optional deadline reset between questions or retries")
		}
		if span.end.After(span.deadline.Add(50 * time.Millisecond)) {
			t.Fatal("provider exceeded shared job deadline")
		}
	}
	if timings[len(timings)-1].end.Sub(timings[0].start) > budget+50*time.Millisecond {
		t.Fatal("total optional provider time exceeded shared budget")
	}
	// Allow 150ms for worker scheduling and the baseline SQL transactions;
	// provider optional work itself is bounded by the exact shared deadline.
	slack := 150 * time.Millisecond
	ceiling := baselineTime + budget + slack
	if end.Sub(start) > ceiling || humanStart.Sub(heldStart) > ceiling || laterStart.Sub(heldStart) > ceiling {
		t.Fatalf("scope held too long: baseline=%s budget=%s job=%s human=%s later=%s", baselineTime, budget, end.Sub(start), humanStart.Sub(heldStart), laterStart.Sub(heldStart))
	}
	if humanStart.Before(end) || laterStart.Before(end) {
		t.Fatal("later work acquired before the serialized job finished")
	}
	var positions []string
	rows, err := w.pool.Query(t.Context(), `SELECT position FROM l2_stances WHERE evidence=ARRAY[$1]::text[] AND assertion IS NULL ORDER BY position`, w.docs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		positions = append(positions, s)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	var baselinePositions []string
	baselineRows, err := baseline.pool.Query(t.Context(), `SELECT position FROM l2_stances WHERE evidence=ARRAY[$1]::text[] AND assertion IS NULL ORDER BY position`, baseline.docs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	for baselineRows.Next() {
		var position string
		if err = baselineRows.Scan(&position); err != nil {
			t.Fatal(err)
		}
		baselinePositions = append(baselinePositions, position)
	}
	baselineRows.Close()
	if err = baselineRows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(positions, baselinePositions) {
		t.Fatalf("extraction changed: %v; baseline %v", positions, baselinePositions)
	}
	// Compare the entire extraction, including the baseline relation values.
	var changed int
	if err = w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_stances WHERE evidence=ARRAY[$1]::text[] AND judgement='changes'`, w.docs[1].ID).Scan(&changed); err != nil || changed != 3 || len(positions) != 3 {
		t.Fatalf("partial boosted batch survived: positions=%v changes=%d %v", positions, changed, err)
	}
	var wrote bool
	if err = w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM l2_stances WHERE position='Later agent write.')`).Scan(&wrote); err != nil || !wrote {
		t.Fatalf("queued write missing: %v", err)
	}
	var gestures int
	if err = w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_gestures WHERE action='demote'`).Scan(&gestures); err != nil || gestures != 1 {
		t.Fatalf("human missing: %d %v", gestures, err)
	}
	var requests, attempts, candidates int64
	if err = w.pool.QueryRow(t.Context(), `SELECT judgment_requests,judgment_attempts,candidate_count FROM pipeline_observations WHERE stage='assert' AND documents @> ARRAY[$1]::text[] ORDER BY recorded_at DESC LIMIT 1`, w.docs[1].ID).Scan(&requests, &attempts, &candidates); err != nil || requests != 7 || attempts != 6 || candidates != 11 {
		t.Fatalf("shared work accounting: requests=%d attempts=%d candidates=%d %v", requests, attempts, candidates, err)
	}
	var exhausted int
	if err = w.pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed' AND fallback_reason='budget_exhausted'`).Scan(&exhausted); err != nil || exhausted < 3 {
		t.Fatalf("remaining questions did not fall back: %d %v", exhausted, err)
	}
	// Human correction is still effective over the completed boosted workflow.
	t.Logf("baseline=%s optional-budget=%s job=%s human-acquired=%s later-write-acquired=%s requests=%d attempts=%d", baselineTime, budget, end.Sub(start), humanStart.Sub(heldStart), laterStart.Sub(heldStart), requests, attempts)
}
