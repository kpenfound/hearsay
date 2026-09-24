//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// The fake registry has no recordings: deleting the root needs no model call.
// The assertion event and withdrawal jobs are processed by the real worker.
func TestSourceTombstoneWithdrawsEvidenceAndKeepsSurvivorsReadable(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	topicID := w.lockTopic()
	graph := l2.New(w.pool)
	before, err := graph.StanceHistory(ctx, topicID)
	if err != nil {
		t.Fatal(err)
	}
	var issueStance, prStance l2.Stance
	for _, st := range before {
		if slices.Equal(st.Evidence, []string{w.issue}) {
			issueStance = st
		}
		if slices.Equal(st.Evidence, []string{w.pr}) {
			prStance = st
		}
	}
	if issueStance.ID == "" || prStance.ID == "" {
		t.Fatalf("fixture stances = %+v", before)
	}

	registry, err := llm.NewFake(llm.Default(), llm.NewFixtures())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Repo = repo(w.src)
	d, err := distiller.New(w.pool, registry, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := assertworker.New(w.pool, registry, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	var asserted api.Asserted
	if err := json.Unmarshal(w.http(t, shedKyle, "assert", map[string]any{
		"topic": topicID, "position": "Both documents support the worker lock.", "evidence": []string{w.issue, w.pr},
	}), &asserted); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(ctx, queue.Job{TargetID: asserted.ID, SerialKey: w.src}); err != nil {
		t.Fatal(err)
	}

	tombstone := func(docID string) {
		t.Helper()
		artifact := strings.TrimPrefix(docID, "l1:"+w.src+":")
		ev := connector.Event{Source: w.src, NativeID: artifact + ":tombstone", Kind: connector.KindTombstone,
			Time: day.Add(10), Payload: connector.Payload{Artifact: artifact + ":tombstone", Target: artifact,
				Container: connector.Container{Kind: connector.ContainerRepository, NativeID: w.project}},
			ACL: connector.ACL{{Kind: connector.ACLPublic}}}
		if _, err := l0.New(w.pool).Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
		result, err := d.Distill(ctx, docID)
		if err != nil || !result.Deleted {
			t.Fatalf("Distill(tombstone) = %+v, %v", result, err)
		}
		if _, err := l1.New(w.pool).Get(ctx, docID); err == nil {
			t.Fatal("the tombstoned L1 document remains")
		}
		rows, err := w.pool.Query(ctx, `SELECT target_id, serial_key FROM queue_job WHERE kind = $1 AND serial_key = $2 AND target_id LIKE 'withdraw:%' AND state = 'pending'`, l2.AssertKindName, w.src)
		if err != nil {
			t.Fatal(err)
		}
		var jobs []queue.Job
		for rows.Next() {
			var job queue.Job
			if err := rows.Scan(&job.TargetID, &job.SerialKey); err != nil {
				t.Fatal(err)
			}
			jobs = append(jobs, job)
		}
		rows.Close()
		if len(jobs) == 0 {
			t.Fatal("deleting L1 enqueued no stance repairs")
		}
		for _, job := range jobs {
			if job.SerialKey != w.src {
				t.Errorf("withdrawal job scope = %q, want topic scope %q", job.SerialKey, w.src)
			}
			if err := a.Handle(ctx, job); err != nil {
				t.Fatal(err)
			}
		}
	}

	tombstone(w.issue)
	h := w.history(t, kyle, topicID)
	withdrawn, retained := false, ""
	for _, st := range h.Stances {
		if st.Position == "evidence deleted" && st.Supersedes == issueStance.ID {
			withdrawn = true
		}
		if st.Position == "Both documents support the worker lock." && slices.Equal(st.Evidence, []string{w.pr}) {
			retained = st.ID
		}
	}
	if !withdrawn || retained == "" || h.Current != retained {
		t.Errorf("history = %+v, want a withdrawal and the surviving assertion evidence", h)
	}
	if got := string(w.http(t, kyle, "get_bundle", map[string]any{"scope": w.scope})); !strings.Contains(got, "Both documents support the worker lock") {
		t.Errorf("bundle after deletion = %s, want surviving position", got)
	}
	if got := string(w.http(t, kyle, "resolve", map[string]any{"text": w.project})); !strings.Contains(got, w.project) {
		t.Errorf("resolve after deletion = %s, want the surviving topic's entity", got)
	}

	tombstone(w.pr)
	tombstone(w.secret)
	if status, _ := w.post(t, "/v1/stance_history", kyle, `{"topic":"`+topicID+`"}`); status != http.StatusNotFound {
		t.Errorf("history without surviving evidence = %d, want 404", status)
	}
}
