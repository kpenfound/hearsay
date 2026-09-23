//go:build integration

package assertworker_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// scratchPool is a migrated database of this test's own.
func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := newPool(t)
	name := "hearsay_assert_" + newSource(t)
	if _, err := admin.Exec(t.Context(), `CREATE DATABASE `+name); err != nil {
		t.Fatalf("creating the scratch database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("dropping the scratch database: %v", err)
		}
	})
	url := os.Getenv("HEARSAY_DATABASE_URL")
	base, query, hasQuery := strings.Cut(url, "?")
	url = base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		url += "?" + query
	}
	migrator, err := db.NewMigrator(t.Context(), url, nil)
	if err != nil {
		t.Fatalf("NewMigrator() = %v", err)
	}
	if _, err := migrator.Up(t.Context()); err != nil {
		t.Fatalf("migrating the scratch database: %v", err)
	}
	_ = migrator.Close()
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to the scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var sources atomic.Int64

// newSource is a source id, and so a scope id, nothing else in the shared
// database uses.
func newSource(t *testing.T) string {
	t.Helper()
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(sources.Add(1), 36)
}

func testConfig(src string) *config.Config {
	cfg := config.Default()
	cfg.Repo = testRepo(src)
	return &cfg
}

// store writes the fixture into L0 and its two documents into L1, the way the
// connectors and the distiller would have.
func store(t *testing.T, pool *pgxpool.Pool, src string) fixture {
	t.Helper()
	f := newFixture(src)
	events := l0.New(pool)
	for _, ev := range append([]connector.Event{f.issue, f.pr}, f.prChildren...) {
		if _, err := events.Append(t.Context(), ev); err != nil {
			t.Fatalf("Append(%s) = %v", ev.NativeID, err)
		}
	}
	issue, pr := documents(t, src)
	for _, doc := range []l1.Document{issue, pr} {
		if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
			t.Fatalf("Put(%s) = %v", doc.ID, err)
		}
	}
	return f
}

func newAsserter(t *testing.T, pool *pgxpool.Pool, src string, fx *llm.Fixtures) *assertworker.Asserter {
	t.Helper()
	registry, err := llm.NewFake(llm.Default(), fx)
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	a, err := assertworker.New(pool, registry, testConfig(src))
	if err != nil {
		t.Fatalf("assertworker.New() = %v", err)
	}
	return a
}

func assertDoc(t *testing.T, a *assertworker.Asserter, docID, scope string) assertworker.Result {
	t.Helper()
	result, err := a.Assert(t.Context(), docID, scope)
	if err != nil {
		t.Fatalf("Assert(%s) = %v", docID, err)
	}
	return result
}

// The acceptance criterion: an issue that proposes X and a later merged pull
// request that does Y yield one topic and two stances, the second superseding
// the first, with evidence pointing at both documents.
func TestAnIssueThenAMergedPullRequestIsOneTopicWithTwoStances(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo)
	if scope != src {
		t.Fatalf("ScopeKey() = %q, want the fixture's own scope %q", scope, src)
	}
	a := newAsserter(t, pool, src, loadFixtures(t))

	if r := assertDoc(t, a, f.issueID, scope); r.TopicsOpened != 1 || r.StancesWritten != 1 {
		t.Fatalf("Assert(issue) = %+v, want one topic opened and one stance", r)
	}
	if r := assertDoc(t, a, f.prID, scope); r.TopicsOpened != 0 || r.StancesWritten != 1 {
		t.Fatalf("Assert(pull request) = %+v, want the topic continued with one stance", r)
	}

	graph := l2.New(pool)
	topics, err := graph.Topics(t.Context(), scope)
	if err != nil {
		t.Fatalf("Topics() = %v", err)
	}
	if len(topics) != 1 {
		t.Fatalf("Topics() = %d topics, want 1: %+v", len(topics), topics)
	}
	topic := topics[0]
	if topic.Name != topicName || topic.OpenedBy != f.issueID {
		t.Errorf("topic = %+v, want %q opened by the issue", topic, topicName)
	}
	for _, want := range []string{f.issueTrackerID, f.prTrackerItemID} {
		if !slices.Contains(topic.About, want) {
			t.Errorf("topic.About = %q, want it to hold %s", topic.About, want)
		}
	}
	for _, want := range []string{"item:" + repo + "#12", "item:" + repo + "#31"} {
		if !slices.Contains(topic.JoinKeys, want) {
			t.Errorf("topic.JoinKeys = %q, want it to hold %s", topic.JoinKeys, want)
		}
	}

	history, err := graph.StanceHistory(t.Context(), topic.ID)
	if err != nil {
		t.Fatalf("StanceHistory() = %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("StanceHistory() = %d stances, want 2: %+v", len(history), history)
	}
	first, second := history[0], history[1]
	if first.Judgement != l2.JudgementUnknown || second.Judgement != l2.JudgementChanges {
		t.Errorf("stance judgements = %q, %q, want unknown then changes", first.Judgement, second.Judgement)
	}
	if read, err := graph.Stance(t.Context(), second.ID); err != nil || read.Judgement != l2.JudgementChanges {
		t.Errorf("Stance() = %+v, %v, want recorded change", read, err)
	}
	if first.Position != issuePosition || !slices.Equal(first.Evidence, []string{f.issueID}) ||
		first.Supersedes != "" || first.Tier != l2.TierInferred || first.Author != "kyle" {
		t.Errorf("first stance = %+v, want the issue's proposal, inferred, superseding nothing, by kyle", first)
	}
	if second.Position != prPosition || !slices.Equal(second.Evidence, []string{f.prID}) ||
		second.Supersedes != first.ID || second.Tier != l2.TierRatified || second.Author != "kyle" {
		t.Errorf("second stance = %+v, want the merged pull request's change, ratified, superseding %s", second, first.ID)
	}

	// The tracker items the two documents name exist as entities now.
	for _, id := range []string{f.issueTrackerID, f.prTrackerItemID} {
		e, err := graph.Entity(t.Context(), id)
		if err != nil || e.Type != l2.TypeTrackerItem || e.Origin != l2.OriginReference {
			t.Errorf("Entity(%s) = %+v, %v, want a tracker item created on reference", id, e, err)
		}
	}
}

// Issue #114: the worker offers a document only the topics, and the positions,
// everyone who may read it may read now — decided by the documents they rest
// on, not the access lists the topic and stance were written with. The fake
// answers only the request recorded here, so a prompt that offered anything
// else would fail the assertion with no fixture.
func TestTheWorkerOffersOnlyWhatTheDocumentsReadersMayReadNow(t *testing.T) {
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: "gh", NativeID: "kyle-node"}}
	answer := func(topic, judgement string) []byte {
		return []byte(`{"assertions":[{"topic":"` + topic + `","topic_name":"` + topicName + `","position":"` + prPosition + `"` + judgement + `}]}`)
	}
	tests := []struct {
		name string
		// hide makes something private after the issue opened its topic.
		hide       func(t *testing.T, pool *pgxpool.Pool, src string, topic l2.Topic)
		candidates []assertworker.Candidate
		response   []byte
		opened     int
	}{
		{
			name: "a topic whose opening document went private is not offered",
			hide: func(t *testing.T, pool *pgxpool.Pool, src string, _ l2.Topic) {
				issue := issueDoc(t, src)
				issue.ACL = private
				if _, err := l1.New(pool).Put(t.Context(), issue); err != nil {
					t.Fatal(err)
				}
			},
			response: answer("new", ""),
			opened:   1,
		},
		{
			name: "a topic whose opening document was retracted is not offered",
			hide: func(t *testing.T, pool *pgxpool.Pool, src string, _ l2.Topic) {
				if _, err := l1.New(pool).Delete(t.Context(), issueDoc(t, src).ID); err != nil {
					t.Fatal(err)
				}
			},
			response: answer("new", ""),
			opened:   1,
		},
		{
			name: "a position resting on a private document is not shown",
			hide: func(t *testing.T, pool *pgxpool.Pool, src string, topic l2.Topic) {
				note := issueDoc(t, src)
				note.Source.NativeID = "a private note"
				note.ID, note.ACL = l1.DocID(src, note.Source.NativeID), private
				if _, err := l1.New(pool).Put(t.Context(), note); err != nil {
					t.Fatal(err)
				}
				// Stated after the issue, so it is the topic's current position.
				stated := note.Time.LastActivity.Add(time.Hour)
				if _, _, err := l2.New(pool).AppendStance(t.Context(), l2.Stance{
					ID: l2.StanceID(topic.ID, note.ID, "what only kyle saw", stated, l2.TierInferred), TopicID: topic.ID,
					Position: "what only kyle saw", StatedAt: stated, Evidence: []string{note.ID, issueDoc(t, src).ID},
					Tier: l2.TierInferred, ACL: connector.ACL{{Kind: connector.ACLPublic}},
				}, stated); err != nil {
					t.Fatal(err)
				}
			},
			candidates: []assertworker.Candidate{{Name: topicName}},
			response:   answer("T1", `,"judgement":"changes"`),
			opened:     0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := newPool(t)
			src := newSource(t)
			f := store(t, pool, src)
			scope := l2.ScopeKey(testRepo(src), src, repo)
			fx := loadFixtures(t)
			if err := fx.Add(llm.CompletionFixture{
				Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), tt.candidates, assertBudget()),
				Response: llm.Response{JSON: tt.response, StopReason: llm.StopEnd, Model: "recorded"},
			}); err != nil {
				t.Fatal(err)
			}
			a := newAsserter(t, pool, src, fx)
			if r := assertDoc(t, a, f.issueID, scope); r.TopicsOpened != 1 {
				t.Fatalf("Assert(issue) = %+v, want a topic opened", r)
			}
			topics, err := l2.New(pool).Topics(t.Context(), scope)
			if err != nil || len(topics) != 1 {
				t.Fatalf("Topics() = %+v, %v, want the issue's", topics, err)
			}
			tt.hide(t, pool, src, topics[0])
			// Were the hidden topic offered as before, the pull request's
			// recorded answer would continue it and open nothing.
			if r := assertDoc(t, a, f.prID, scope); r.TopicsOpened != tt.opened || r.StancesWritten != 1 {
				t.Errorf("Assert(pull request) = %+v, want %d topics opened and one stance", r, tt.opened)
			}
			// A judgement against a position the model was not shown is no
			// comparison, and is not recorded as one.
			written, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			if err != nil || len(written) != 1 || written[0].Judgement != l2.JudgementUnknown {
				t.Errorf("StancesFrom(pull request) = %+v, %v, want one stance with no judgement", written, err)
			}
		})
	}
}

func TestExistingTopicRequiresJudgement(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{"missing", `{"assertions":[{"topic":"T1","topic_name":"` + topicName + `","position":"` + prPosition + `"}]}`},
		{"new topic with judgement", `{"assertions":[{"topic":"new","topic_name":"another question","position":"another answer","judgement":"changes"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newPool(t)
			src := newSource(t)
			f := store(t, pool, src)
			fx := llm.NewFixtures()
			issue := issueDoc(t, src)
			if err := fx.Add(llm.CompletionFixture{
				Tier:     llm.TierAssert,
				Request:  assertworker.RequestFor(issue, nil, assertBudget()),
				Response: llm.Response{JSON: []byte(`{"assertions":[{"topic":"new","topic_name":"` + topicName + `","position":"` + issuePosition + `"}]}`), StopReason: llm.StopEnd, Model: "recorded"},
			}); err != nil {
				t.Fatal(err)
			}
			pr := prDoc(t, src)
			if err := fx.Add(llm.CompletionFixture{
				Tier:     llm.TierAssert,
				Request:  assertworker.RequestFor(pr, []assertworker.Candidate{{Name: topicName, Current: issuePosition}}, assertBudget()),
				Response: llm.Response{JSON: []byte(tc.json), StopReason: llm.StopEnd, Model: "recorded"},
			}); err != nil {
				t.Fatal(err)
			}
			a := newAsserter(t, pool, src, fx)
			assertDoc(t, a, f.issueID, src)
			if _, err := a.Assert(t.Context(), f.prID, src); err == nil || !strings.Contains(err.Error(), "invalid judgement") {
				t.Errorf("Assert(malformed answer) = %v, want judgement error", err)
			}
			if topics, err := l2.New(pool).Topics(t.Context(), src); err != nil || len(topics) != 1 {
				t.Errorf("Topics() after rejected response = %+v, %v, want only the issue's topic", topics, err)
			}
		})
	}
}

func TestAProposedPullRequestBecomesRatifiedWithTheSamePosition(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	merged := prDoc(t, src)
	proposal := prBody
	proposal.OutcomeKind = l1.OutcomeProposed
	proposal.Outcome = "Proposed, pending review."
	proposed, _, err := merged.WithBody(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := l1.New(pool).Put(t.Context(), proposed); err != nil || !changed {
		t.Fatalf("Put(proposed PR) = %v, %v", changed, err)
	}
	fx := loadFixtures(t)
	if err := fx.Add(llm.CompletionFixture{
		Tier: llm.TierAssert, Request: assertworker.RequestFor(proposed, nil, assertBudget()),
		Response: llm.Response{JSON: []byte(`{"assertions":[{"topic":"new","topic_name":"` + topicName + `","position":"` + prPosition + `"}]}`),
			StopReason: llm.StopEnd, Model: "recorded"},
	}); err != nil {
		t.Fatalf("recording the proposed PR reading: %v", err)
	}
	scope := l2.ScopeKey(testRepo(src), src, repo)
	a := newAsserter(t, pool, src, fx)
	if r := assertDoc(t, a, f.prID, scope); r.StancesWritten != 1 || r.TopicsOpened != 1 {
		t.Fatalf("Assert(proposed PR) = %+v, want one inferred stance", r)
	}
	if changed, err := l1.New(pool).Put(t.Context(), merged); err != nil || !changed {
		t.Fatalf("Put(merged PR) = %v, %v", changed, err)
	}
	if r := assertDoc(t, a, f.prID, scope); r.StancesWritten != 1 || r.TopicsOpened != 0 {
		t.Fatalf("Assert(merged PR) = %+v, want one new ratified stance", r)
	}
	graph := l2.New(pool)
	topics, err := graph.Topics(t.Context(), scope)
	if err != nil || len(topics) != 1 {
		t.Fatalf("Topics() = %+v, %v, want one topic", topics, err)
	}
	history, err := graph.StanceHistory(t.Context(), topics[0].ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("StanceHistory() = %+v, %v, want both readings", history, err)
	}
	if history[0].Position != prPosition || history[0].Tier != l2.TierInferred ||
		history[1].Position != prPosition || history[1].Tier != l2.TierRatified ||
		history[1].ID == history[0].ID || history[1].Supersedes != history[0].ID {
		t.Errorf("PR history = %+v, want inferred then ratified at the same position", history)
	}
	if current, ok := l2.Current(history); !ok || current.ID != history[1].ID {
		t.Errorf("Current() = %+v, %v, want the ratified stance", current, ok)
	}
	if r := assertDoc(t, a, f.prID, scope); !r.Unchanged || r.StancesWritten != 0 {
		t.Errorf("retry of merged version = %+v, want nothing written", r)
	}
}

// The other acceptance criterion: re-running over the same documents — a
// restart, a job run twice — does not duplicate stances, and does not ask the
// model again.
func TestTheWorkerIsIdempotentAcrossRestarts(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo)
	first := newAsserter(t, pool, src, loadFixtures(t))
	assertDoc(t, first, f.issueID, scope)
	assertDoc(t, first, f.prID, scope)

	// A new process, with a registry that has nothing recorded: any model call
	// fails, so passing here means none was made.
	restarted := newAsserter(t, pool, src, llm.NewFixtures())
	for _, id := range []string{f.issueID, f.prID} {
		if r := assertDoc(t, restarted, id, scope); !r.Unchanged || r.StancesWritten != 0 || r.TopicsOpened != 0 {
			t.Errorf("Assert(%s) after a restart = %+v, want it unchanged and nothing written", id, r)
		}
	}
	assertOneTopicTwoStances(t, pool, scope)

	pending, err := l2.New(pool).Unasserted(t.Context())
	if err != nil {
		t.Fatalf("Unasserted() = %v", err)
	}
	for _, p := range pending {
		if p.ID == f.issueID || p.ID == f.prID {
			t.Errorf("Unasserted() lists %s, which has been read", p.ID)
		}
	}
}

// Losing the assertion checkpoint makes the same L1 version run again. It
// asks the model, but the identical reading writes no second stance.
func TestRetryingTheSameVersionWritesNothingTwice(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo)
	a := newAsserter(t, pool, src, loadFixtures(t))
	assertDoc(t, a, f.issueID, scope)

	if _, err := pool.Exec(t.Context(),
		`UPDATE l2_asserted SET distilled_at = distilled_at - interval '1 second' WHERE doc_id = $1`, f.issueID); err != nil {
		t.Fatalf("moving the recorded version: %v", err)
	}

	// The sweep finds it again, and with nothing recorded the read must fail:
	// the version moved, so it is read again rather than skipped.
	pending, err := l2.New(pool).Unasserted(t.Context())
	if err != nil {
		t.Fatalf("Unasserted() = %v", err)
	}
	if !slices.ContainsFunc(pending, func(p l2.Pending) bool { return p.ID == f.issueID }) {
		t.Errorf("Unasserted() does not list %s, whose version moved", f.issueID)
	}
	if _, err := newAsserter(t, pool, src, llm.NewFixtures()).Assert(t.Context(), f.issueID, scope); !errors.Is(err, llm.ErrNoFixture) {
		t.Fatalf("Assert(a new version) with no recordings = %v, want it to have asked the model", err)
	}

	r := assertDoc(t, a, f.issueID, scope)
	if r.Unchanged || r.Positions != 1 || r.StancesWritten != 0 || r.TopicsOpened != 0 {
		t.Errorf("Assert(again) = %+v, want one position read and nothing new written", r)
	}
	topics, err := l2.New(pool).Topics(t.Context(), scope)
	if err != nil || len(topics) != 1 {
		t.Fatalf("Topics() = %+v, %v, want the one topic", topics, err)
	}
	history, err := l2.New(pool).StanceHistory(t.Context(), topics[0].ID)
	if err != nil || len(history) != 1 {
		t.Errorf("StanceHistory() = %+v, %v, want the one stance", history, err)
	}
	if again, ok, err := l2.New(pool).Asserted(t.Context(), f.issueID); err != nil || !ok {
		t.Errorf("Asserted() = %v, %v, %v, want the new version recorded", again, ok, err)
	} else if r := assertDoc(t, a, f.issueID, scope); !r.Unchanged {
		t.Errorf("Assert(a third time) = %+v, want it unchanged", r)
	}
}

// The #51 fixture plus one comment (#72): the issue proposes (S1), the merged
// pull request does otherwise (S2), and the issue, commented on after the merge
// and re-distilled, is read again and restated in other words (S3). S3 replaces
// the issue's own S1; it is not recorded as reversing the merged pull request,
// and the issue is left with one live stance on the topic.
func TestARedistilledDocumentReplacesItsOwnStance(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo)
	a := newAsserter(t, pool, src, loadFixtures(t))
	assertDoc(t, a, f.issueID, scope)
	assertDoc(t, a, f.prID, scope)

	comment := issueComment(src)
	if _, err := l0.New(pool).Append(t.Context(), comment); err != nil {
		t.Fatalf("Append(comment) = %v", err)
	}
	commented := commentedIssueDoc(t, src)
	if changed, err := l1.New(pool).Put(t.Context(), commented); err != nil || !changed {
		t.Fatalf("Put(the commented issue) = %v, %v, want a new version", changed, err)
	}
	if r := assertDoc(t, a, f.issueID, scope); r.Unchanged || r.StancesWritten != 1 || r.TopicsOpened != 0 {
		t.Fatalf("Assert(the commented issue) = %+v, want the topic continued with one new stance", r)
	}

	graph := l2.New(pool)
	topics, err := graph.Topics(t.Context(), scope)
	if err != nil || len(topics) != 1 {
		t.Fatalf("Topics() = %+v, %v, want the one topic", topics, err)
	}
	history, err := graph.StanceHistory(t.Context(), topics[0].ID)
	if err != nil || len(history) != 3 {
		t.Fatalf("StanceHistory() = %+v, %v, want three stances", history, err)
	}
	s1, s2, s3 := history[0], history[1], history[2]
	if s3.Judgement != l2.JudgementChanges {
		t.Errorf("restated issue against current PR = %q, want changes", s3.Judgement)
	}
	if s1.Position != issuePosition || s2.Position != prPosition || s3.Position != restatedPosition {
		t.Fatalf("StanceHistory() positions = %q, %q, %q", s1.Position, s2.Position, s3.Position)
	}
	if s2.Supersedes != s1.ID {
		t.Errorf("the pull request's stance supersedes %q, want the issue's first %q", s2.Supersedes, s1.ID)
	}
	if s3.Supersedes != s1.ID {
		t.Errorf("the issue's restatement supersedes %q, want the issue's own earlier stance %q, not the merged pull request's", s3.Supersedes, s1.ID)
	}
	if !slices.Equal(s3.Evidence, []string{f.issueID}) || s3.Tier != l2.TierInferred {
		t.Errorf("the restatement = %+v, want the issue's, inferred", s3)
	}
	superseded := map[string]bool{}
	for _, st := range history {
		superseded[st.Supersedes] = true
	}
	live := 0
	for _, st := range history {
		if st.Evidence[0] == f.issueID && !superseded[st.ID] {
			live++
		}
	}
	if live != 1 {
		t.Errorf("the issue has %d unsuperseded stances on the topic, want 1", live)
	}

	// The comment is deleted and the issue re-distilled without it, so its
	// last activity moves back before the merge. Read again (S4), it replaces
	// S3 and retires it, though S3 is still the newest stated on the topic.
	tombstone := event(src, connector.KindTombstone, comment.NativeID+":tombstone", at(9), "", "", "", "")
	tombstone.Payload.Author, tombstone.Payload.Target = nil, comment.NativeID
	if _, err := l0.New(pool).Append(t.Context(), tombstone); err != nil {
		t.Fatalf("Append(tombstone) = %v", err)
	}
	uncommented := issueDoc(t, src)
	if changed, err := l1.New(pool).Put(t.Context(), uncommented); err != nil || !changed {
		t.Fatalf("Put(the issue without its comment) = %v, %v, want a new version", changed, err)
	}
	if r := assertDoc(t, a, f.issueID, scope); r.StancesWritten != 1 {
		t.Fatalf("Assert(the issue without its comment) = %+v, want one new stance", r)
	}
	after, err := graph.StanceHistory(t.Context(), topics[0].ID)
	if err != nil || len(after) != 4 {
		t.Fatalf("StanceHistory() = %+v, %v, want four stances", after, err)
	}
	if last := after[len(after)-1]; last.ID != s3.ID {
		t.Fatalf("the newest stated stance is %q, want the retired restatement %q", last.Position, s3.Position)
	}
	foundParaphrase := false
	for _, st := range after {
		if st.Position == uncommentedPosition {
			foundParaphrase = true
			if st.Judgement != l2.JudgementRestates {
				t.Errorf("paraphrase judgement = %q, want restates", st.Judgement)
			}
		}
	}
	if !foundParaphrase {
		t.Errorf("StanceHistory() = %+v, want the paraphrase", after)
	}

	// The pull request read again is shown the topic at its current stance,
	// the merged pull request's own, not at the newest stated. Only the
	// recording keyed on that candidate exists, so showing the retired
	// restatement fails the call.
	if _, err := pool.Exec(t.Context(),
		`UPDATE l2_asserted SET distilled_at = distilled_at - interval '1 second' WHERE doc_id = $1`, f.prID); err != nil {
		t.Fatalf("moving the recorded version: %v", err)
	}
	if r := assertDoc(t, a, f.prID, scope); r.Positions != 1 || r.StancesWritten != 0 {
		t.Errorf("Assert(the pull request again) = %+v, want its one position, already stored", r)
	}
}

// What a model writes is held to what L1 holds a person's words to: an email
// address is scrubbed out of a position, and an assertion with nothing left in
// its position, or a new topic with nothing left in its name, is dropped rather
// than failing the document.
func TestWhatTheModelWroteIsCleanedBeforeItIsStored(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	doc := commitDocument(t, src)
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	r := assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), doc.ID, src)
	if r.Positions != 1 || r.TopicsOpened != 1 || r.StancesWritten != 1 {
		t.Fatalf("Assert(commit) = %+v, want one position kept of three", r)
	}
	stances, err := l2.New(pool).StancesFrom(t.Context(), doc.ID)
	if err != nil || len(stances) != 1 {
		t.Fatalf("StancesFrom() = %+v, %v, want one", stances, err)
	}
	if strings.Contains(stances[0].Position, commitEmail) || !strings.Contains(stances[0].Position, "lock order") {
		t.Errorf("stored position = %q, want the rest of it without %s", stances[0].Position, commitEmail)
	}
	if stances[0].Tier != l2.TierInferred {
		t.Errorf("a resolved commit is %s, want inferred: only a merged pull request is ratified", stances[0].Tier)
	}
}

// A document whose outcome does not enter the pipeline, and one that is gone,
// are skipped without a model call.
func TestDocumentsOutsideThePipelineAreSkipped(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	store(t, pool, src)
	issue, _ := documents(t, src)
	open, _, err := issue.WithBody(l1.Body{Summary: "Still being worked out.", OutcomeKind: l1.OutcomeOpen})
	if err != nil {
		t.Fatalf("WithBody() = %v", err)
	}
	if _, err := l1.New(pool).Put(t.Context(), open); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	a := newAsserter(t, pool, src, llm.NewFixtures())
	for _, id := range []string{open.ID, l1.DocID(src, repo+"#999")} {
		if r := assertDoc(t, a, id, src); !r.Skipped {
			t.Errorf("Assert(%s) = %+v, want it skipped", id, r)
		}
	}
}

// The service end to end: jobs the distiller would have enqueued, and documents
// it wrote before the worker existed, are all read by Run, which stops cleanly.
//
// It runs on a database of its own: Run's sweep enqueues every unread document
// it can see, and on the shared database that is every other test's, which its
// worker would then read while those tests are reading them too.
func TestRunReadsWhatIsEnqueuedAndWhatWasMissed(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	store(t, pool, src)
	issue, _ := documents(t, src)

	// The issue's job, as the distiller enqueues it; the pull request has none,
	// and is only found by the sweep at startup.
	if ok, err := l2.EnqueueAssertion(t.Context(), pool, testRepo(src), issue, repo); err != nil || !ok {
		t.Fatalf("EnqueueAssertion() = %v, %v", ok, err)
	}

	registry, err := llm.NewFake(llm.Default(), loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- assertworker.Run(ctx, testConfig(src), assertworker.Deps{Pool: pool, LLM: registry}) }()

	waitFor(t, "both documents to be read", func() bool {
		topics, err := l2.New(pool).Topics(t.Context(), src)
		if err != nil || len(topics) != 1 {
			return false
		}
		history, err := l2.New(pool).StanceHistory(t.Context(), topics[0].ID)
		return err == nil && len(history) == 2
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() after cancellation = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return within 10s of cancellation")
	}
	assertOneTopicTwoStances(t, pool, src)
}

// The distiller writes a document and its assert job together, and only for an
// outcome that enters the pipeline.
func TestEnqueueAssertionOnlyForOutcomesThatAssert(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	issue, _ := documents(t, src)
	open, _, err := issue.WithBody(l1.Body{Summary: "Open.", OutcomeKind: l1.OutcomeOpen})
	if err != nil {
		t.Fatalf("WithBody() = %v", err)
	}
	if ok, err := l2.EnqueueAssertion(t.Context(), pool, testRepo(src), open, repo); err != nil || ok {
		t.Errorf("EnqueueAssertion(open) = %v, %v, want no job", ok, err)
	}
}

func assertOneTopicTwoStances(t *testing.T, pool *pgxpool.Pool, scope string) {
	t.Helper()
	graph := l2.New(pool)
	topics, err := graph.Topics(t.Context(), scope)
	if err != nil || len(topics) != 1 {
		t.Fatalf("Topics() = %+v, %v, want exactly one", topics, err)
	}
	history, err := graph.StanceHistory(t.Context(), topics[0].ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("StanceHistory() = %+v, %v, want exactly two", history, err)
	}
	if history[1].Supersedes != history[0].ID {
		t.Errorf("the second stance supersedes %q, want %q", history[1].Supersedes, history[0].ID)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
