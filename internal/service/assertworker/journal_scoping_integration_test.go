//go:build integration

package assertworker_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
)

func TestStanceJournalReusedAcrossConcurrentUnrelatedIngestion(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	cfg := judgmentConfig()
	// Count every provider request, including the topic and shortlist shadow
	// judgments, so a restart cannot silently request either one again.
	capability := func(fake *llm.FakeJudgmentProvider) llm.Judgments {
		j, err := llm.NewJudgments(cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fixture-key" })
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	fake := llm.NewFakeJudgmentProvider(
		llm.JudgmentStep{Response: judgmentFixture(t, "restates")},
		llm.JudgmentStep{Response: topicFixture(t, "shortlist-none-1")},
		llm.JudgmentStep{Response: topicFixture(t, "topic-none-1")},
	)
	j := capability(fake)
	a := boostedAsserter(t, pool, src, cfg, j, loadFixtures(t))
	var topicsBefore []byte
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(t)) FROM l2_topics t`).Scan(&topicsBefore); err != nil {
		t.Fatal(err)
	}
	committed := make(map[string]string)
	a.WithJournalCommit(func(ctx context.Context, tx pgx.Tx, rs []l1.JudgmentRecord, obs []l1.PipelineObservation, opts l1.JournalCommitOptions) error {
		if len(rs) == 0 {
			return l1.CommitJudgments(ctx, tx, rs, obs, opts)
		}
		if len(rs) != 3 {
			t.Fatalf("judgments=%d", len(rs))
		}
		for _, r := range rs {
			if r.Task == string(llm.TaskStanceComparison) && (r.Label != "restates" || r.Mode != "accepted" || r.Outcome != "answer") {
				t.Fatalf("stance judgment=%+v", r)
			}
			committed[r.Task] = r.ID
		}
		for _, task := range []llm.JudgmentTask{llm.TaskStanceComparison, llm.TaskTopicShortlist, llm.TaskTopicMatching} {
			if committed[string(task)] == "" {
				t.Fatalf("missing judgment for %s", task)
			}
		}
		// Hold the job just before validation while another connection ingests an
		// unrelated document in the same source. Its markers must not refuse this
		// accepted batch or change the identity used after restart.
		ingested := make(chan error, 1)
		go func() {
			_, err := pool.Exec(ctx, `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl)
     VALUES($1,$2,'unrelated','issue','unrelated',now(),'{"artifact":"unrelated","title":"Separate issue","text":"Separate discussion."}','[{"kind":"public"}]')`, "evt:"+src+":unrelated", src)
			ingested <- err
		}()
		if err := <-ingested; err != nil {
			t.Fatal(err)
		}
		return l1.CommitJudgments(ctx, tx, rs, obs, opts)
	})
	assertDoc(t, a, f.prID, src)
	writtenJudgment(t, pool, f.prID, l2.JudgementRestates)
	if fake.Calls() != 3 {
		t.Fatalf("calls=%d", fake.Calls())
	}
	checkCommitted := func() {
		t.Helper()
		for task, id := range committed {
			records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), task)
			if err != nil || len(records) != 1 || records[0].ID != id || records[0].State != "committed" {
				t.Fatalf("task=%s records=%+v %v", task, records, err)
			}
		}
	}
	checkCommitted()
	// Recreate the pre-output effective state to model recovery before the job's
	// consumer writes. This exercises reuse through the worker's journal lookup,
	// rather than its already-asserted fast path.
	for _, sql := range []string{`DELETE FROM l2_stances WHERE $1=ANY(evidence)`, `DELETE FROM l2_asserted WHERE doc_id=$1`} {
		if _, err := pool.Exec(t.Context(), sql, f.prID); err != nil {
			t.Fatal(err)
		}
	}
	// The job also extends the candidate's join keys and entity references.
	// Restore those inputs exactly; changing a candidate must invalidate reuse.
	if _, err := pool.Exec(t.Context(), `UPDATE l2_topics t SET about=b.about,join_keys=b.join_keys
      FROM jsonb_populate_recordset(NULL::l2_topics,$1) b WHERE t.id=b.id`, topicsBefore); err != nil {
		t.Fatal(err)
	}
	restartFake := llm.NewFakeJudgmentProvider()
	restartJ := capability(restartFake)
	restarted := boostedAsserter(t, pool, src, cfg, restartJ, loadFixtures(t))
	reused := make(map[string]bool)
	restarted.WithJournalCommit(func(ctx context.Context, tx pgx.Tx, rs []l1.JudgmentRecord, obs []l1.PipelineObservation, opts l1.JournalCommitOptions) error {
		for _, o := range obs {
			if o.JudgmentRequests == nil || *o.JudgmentRequests != 0 || o.JudgmentAttempts == nil || *o.JudgmentAttempts != 0 || o.JudgmentFallbacks == nil || *o.JudgmentFallbacks != 0 {
				t.Fatalf("reused results counted again: %+v", o)
			}
		}
		for _, r := range rs {
			if r.ID != committed[r.Task] || r.State != "committed" {
				t.Fatalf("restart did not reuse committed %s judgment: %+v", r.Task, r)
			}
			reused[r.Task] = true
		}
		return l1.CommitJudgments(ctx, tx, rs, obs, opts)
	})
	assertDoc(t, restarted, f.prID, src)
	writtenJudgment(t, pool, f.prID, l2.JudgementRestates)
	if restartFake.Calls() != 0 {
		t.Fatal("restart requested a new judgment")
	}
	for task := range committed {
		if !reused[task] {
			t.Errorf("restart did not reuse %s judgment", task)
		}
	}
	checkCommitted()
}
