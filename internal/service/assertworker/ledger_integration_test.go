//go:build integration

package assertworker_test

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

// Issue #172: a document whose join keys find a topic that was merged away is
// offered the topic it went into, with that topic's current position, and its
// stance is written there: no duplicate topic, and nothing on the merged-away
// row. The fake answers only the requests recorded here, so a prompt that
// offered the merged-away topic would fail with no fixture.
func TestMatchingReusesTheTopicAMergeKept(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo)
	issue, pr := issueDoc(t, src), prDoc(t, src)
	const keptName, keptPosition = "How the engine serializes its writes", "The engine serializes writes behind one lock."
	fx := llm.NewFixtures()
	for _, fixture := range []llm.CompletionFixture{
		{
			Tier: llm.TierAssert, Request: assertworker.RequestFor(issue, nil, assertBudget()),
			Response: llm.Response{JSON: []byte(`{"assertions":[{"topic":"new","topic_name":"` + topicName + `","position":"` + issuePosition + `"}]}`), StopReason: llm.StopEnd, Model: "recorded"},
		},
		{
			Tier: llm.TierAssert, Request: assertworker.RequestFor(pr, []assertworker.Candidate{{Name: keptName, Current: keptPosition}}, assertBudget()),
			Response: llm.Response{JSON: []byte(`{"assertions":[{"topic":"T1","topic_name":"` + keptName + `","position":"` + prPosition + `","judgement":"changes"}]}`), StopReason: llm.StopEnd, Model: "recorded"},
		},
	} {
		if err := fx.Add(fixture); err != nil {
			t.Fatal(err)
		}
	}
	a := newAsserter(t, pool, src, fx)
	if r := assertDoc(t, a, f.issueID, scope); r.TopicsOpened != 1 {
		t.Fatalf("Assert(issue) = %+v, want a topic opened", r)
	}
	graph := l2.New(pool)
	opened, err := graph.Topics(t.Context(), scope)
	if err != nil || len(opened) != 1 {
		t.Fatalf("Topics() = %+v, %v, want the issue's", opened, err)
	}
	// Another topic, which the issue's is merged into. It shares no join key
	// with the pull request: only the merge leads there.
	kept := l2.Topic{ID: l2.TopicID(scope, f.issueID, 9, keptName), Scope: scope, Name: keptName,
		ACL: connector.ACL{{Kind: connector.ACLPublic}}, OpenedBy: f.issueID}
	if _, err := graph.OpenTopic(t.Context(), kept); err != nil {
		t.Fatal(err)
	}
	stated := issue.Time.LastActivity.Add(time.Minute)
	if _, _, err := graph.AppendStance(t.Context(), l2.Stance{
		ID: l2.StanceID(kept.ID, f.issueID, keptPosition, stated, l2.TierInferred), TopicID: kept.ID, Position: keptPosition,
		StatedAt: stated, Evidence: []string{f.issueID}, Tier: l2.TierInferred, ACL: kept.ACL,
	}, stated); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := l2.Operate(ctx, pool, testRepo(src), l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: kept.ID, From: opened[0].ID}); err != nil {
		t.Fatal(err)
	}

	if r := assertDoc(t, a, f.prID, scope); r.TopicsOpened != 0 || r.StancesWritten != 1 {
		t.Fatalf("Assert(pull request) = %+v, want no topic opened and one stance", r)
	}
	written, err := graph.StancesFrom(t.Context(), f.prID)
	if err != nil || len(written) != 1 || written[0].TopicID != kept.ID || written[0].Judgement != l2.JudgementChanges {
		t.Fatalf("StancesFrom(pull request) = %+v, %v, want one stance on %s, judged against the kept position", written, err, kept.ID)
	}
	rows, err := pool.Query(t.Context(), `SELECT topic_id, count(*) FROM l2_stances WHERE topic_id = ANY($1) GROUP BY topic_id`, []string{kept.ID, opened[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	onRows := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		onRows[id] = n
	}
	rows.Close()
	if onRows[opened[0].ID] != 1 || onRows[kept.ID] != 2 {
		t.Errorf("stance rows = %v, want the issue's alone on the merged-away topic and the pull request's on the kept one", onRows)
	}
	if topics, err := graph.Topics(t.Context(), scope); err != nil || len(topics) != 1 || topics[0].ID != kept.ID {
		t.Errorf("Topics() = %+v, %v, want only the kept topic", topics, err)
	}
}

// A document read again whose answer opens the topic it opened the first time
// — an id derived from the document and the answer — writes where that topic
// was merged, superseding its own earlier reading there, rather than on the
// merged-away row.
func TestAReadingThatReopensAMergedTopicWritesWhereItWent(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo)
	issue := issueDoc(t, src)
	const keptName = "How the engine serializes its writes"
	opens := llm.Response{JSON: []byte(`{"assertions":[{"topic":"new","topic_name":"` + topicName + `","position":"` + issuePosition + `"}]}`), StopReason: llm.StopEnd, Model: "recorded"}
	fx := llm.NewFixtures()
	for _, fixture := range []llm.CompletionFixture{
		{Tier: llm.TierAssert, Request: assertworker.RequestFor(issue, nil, assertBudget()), Response: opens},
		{Tier: llm.TierAssert, Request: assertworker.RequestFor(issue, []assertworker.Candidate{{Name: keptName, Current: issuePosition}}, assertBudget()), Response: opens},
	} {
		if err := fx.Add(fixture); err != nil {
			t.Fatal(err)
		}
	}
	a := newAsserter(t, pool, src, fx)
	assertDoc(t, a, f.issueID, scope)
	graph := l2.New(pool)
	first, err := graph.StancesFrom(t.Context(), f.issueID)
	if err != nil || len(first) != 1 {
		t.Fatalf("StancesFrom(issue) = %+v, %v, want one", first, err)
	}
	merged := first[0].TopicID
	kept := l2.Topic{ID: l2.TopicID(scope, f.issueID, 9, keptName), Scope: scope, Name: keptName,
		ACL: connector.ACL{{Kind: connector.ACLPublic}}, OpenedBy: f.issueID}
	if _, err := graph.OpenTopic(t.Context(), kept); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := l2.Operate(ctx, pool, testRepo(src), l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: kept.ID, From: merged}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE l2_asserted SET distilled_at = distilled_at - interval '1 second' WHERE doc_id = $1`, f.issueID); err != nil {
		t.Fatalf("moving the recorded version: %v", err)
	}

	if r := assertDoc(t, a, f.issueID, scope); r.TopicsOpened != 0 || r.StancesWritten != 1 {
		t.Fatalf("Assert(the issue again) = %+v, want no topic opened and one stance", r)
	}
	var row string
	if err := pool.QueryRow(t.Context(), `SELECT topic_id FROM l2_stances WHERE evidence[1] = $1 AND supersedes = $2`,
		f.issueID, first[0].ID).Scan(&row); err != nil || row != kept.ID {
		t.Errorf("the new reading is on row %q (%v), want %s superseding the first reading", row, err, kept.ID)
	}
	if history, err := graph.StanceHistory(t.Context(), merged); err != nil || len(history) != 2 || !history[0].Retired || history[1].TopicID != kept.ID {
		t.Errorf("StanceHistory(the merged-away topic) = %+v, %v, want the first reading retired and the new one on %s", history, err, kept.ID)
	}
}
