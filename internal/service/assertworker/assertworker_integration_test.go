//go:build integration

package assertworker_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/poll"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

func testConfig(src string) *config.Config {
	cfg := config.Default()
	cfg.Repo = testRepo(src)
	return &cfg
}

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
	return newJevAsserter(t, pool, src, fx, newRuleJev())
}

func assertDoc(t *testing.T, a *assertworker.Asserter, docID, scope string) assertworker.Result {
	t.Helper()
	result, err := a.Assert(t.Context(), docID, scope)
	if err != nil {
		t.Fatalf("Assert(%s) = %v", docID, err)
	}
	return result
}

func TestAnIssueThenAMergedPullRequestIsOneTopicWithTwoStances(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo, nil)
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

	for _, id := range []string{f.issueTrackerID, f.prTrackerItemID} {
		e, err := graph.Entity(t.Context(), id)
		if err != nil || e.Type != l2.TypeTrackerItem || e.Origin != l2.OriginReference {
			t.Errorf("Entity(%s) = %+v, %v, want a tracker item created on reference", id, e, err)
		}
	}
}

func TestHandlingAJobLogsIdsAndNoContentAtInfoLevel(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo, nil)
	a := newAsserter(t, pool, src, loadFixtures(t))

	var logs bytes.Buffer
	ctx := telemetry.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := a.Handle(ctx, queue.Job{Kind: l2.AssertKind(), TargetID: f.issueID, SerialKey: scope}); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "document asserted") || !strings.Contains(out, f.issueID) {
		t.Fatalf("the assertion was not logged by id:\n%s", out)
	}
	stored, err := l1.New(pool).Get(t.Context(), f.issueID)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"the raw text": stored.RawText, "the distilled text": stored.Text, "the position": issuePosition, "the topic name": topicName} {
		if content != "" && strings.Contains(out, content) {
			t.Errorf("info-level output carries %s:\n%s", name, out)
		}
	}
}

func TestTheWorkerOffersOnlyWhatTheDocumentsReadersMayReadNow(t *testing.T) {
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: "gh", NativeID: "kyle-node"}}
	tests := []struct {
		name string

		hide func(t *testing.T, pool *pgxpool.Pool, src string, topic l2.Topic)

		shown  []shownCandidate
		opened int
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
			opened: 1,
		},
		{
			name: "a topic whose opening document was retracted is not offered",
			hide: func(t *testing.T, pool *pgxpool.Pool, src string, _ l2.Topic) {
				if _, err := l1.New(pool).Delete(t.Context(), issueDoc(t, src).ID); err != nil {
					t.Fatal(err)
				}
			},
			opened: 1,
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

				stated := note.Time.LastActivity.Add(time.Hour)
				if _, _, err := l2.New(pool).AppendStance(t.Context(), l2.Stance{
					ID: l2.StanceID(topic.ID, note.ID, "what only kyle saw", stated, l2.TierInferred), TopicID: topic.ID,
					Position: "what only kyle saw", StatedAt: stated, Evidence: []string{note.ID, issueDoc(t, src).ID},
					Tier: l2.TierInferred, ACL: connector.ACL{{Kind: connector.ACLPublic}},
				}, stated); err != nil {
					t.Fatal(err)
				}
			},
			shown:  []shownCandidate{{Label: "T1", Name: topicName}},
			opened: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := dbtest.Pool(t)
			src := dbtest.UniqueID()
			f := store(t, pool, src)
			scope := l2.ScopeKey(testRepo(src), src, repo, nil)
			jev := newRuleJev()
			a := newJevAsserter(t, pool, src, loadFixtures(t), jev)
			if r := assertDoc(t, a, f.issueID, scope); r.TopicsOpened != 1 {
				t.Fatalf("Assert(issue) = %+v, want a topic opened", r)
			}
			topics, err := l2.New(pool).Topics(t.Context(), scope)
			if err != nil || len(topics) != 1 {
				t.Fatalf("Topics() = %+v, %v, want the issue's", topics, err)
			}
			tt.hide(t, pool, src, topics[0])

			if r := assertDoc(t, a, f.prID, scope); r.TopicsOpened != tt.opened || r.StancesWritten != 1 {
				t.Errorf("Assert(pull request) = %+v, want %d topics opened and one stance", r, tt.opened)
			}
			var got []shownCandidate
			for _, req := range jev.asked("same_question") {
				got = append(got, shown(t, req)...)
			}
			if !slices.Equal(got, tt.shown) {
				t.Errorf("Jev was shown %+v, want %+v", got, tt.shown)
			}

			if n := len(jev.asked("same_answer")); n != 0 {
				t.Errorf("Jev was asked %d stance comparisons, want none", n)
			}
			written, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			if err != nil || len(written) != 1 || written[0].Judgement != l2.JudgementUnknown {
				t.Errorf("StancesFrom(pull request) = %+v, %v, want one stance with no judgement", written, err)
			}
		})
	}
}

func TestAProposedPullRequestBecomesRatifiedWithTheSamePosition(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	f := store(t, pool, src)
	merged := prDoc(t, src)
	proposal := prBody
	proposal.OutcomeKind = l1.OutcomeProposed
	proposal.Outcome = "Proposed, pending review."
	proposed, _, err := merged.WithBody(proposal)
	if err != nil {
		t.Fatal(err)
	}

	proposed.ArtifactClass = config.ArtifactPullRequest
	if changed, err := l1.New(pool).Put(t.Context(), proposed); err != nil || !changed {
		t.Fatalf("Put(proposed PR) = %v, %v", changed, err)
	}
	fx := loadFixtures(t)
	if err := fx.Add(llm.CompletionFixture{
		Tier: llm.TierAssert, Request: assertworker.RequestFor(proposed, assertBudget()),
		Response: llm.Response{JSON: []byte(`{"assertions":[{"topic_name":"` + topicName + `","position":"` + prPosition + `"}]}`),
			StopReason: llm.StopEnd, Model: "recorded"},
	}); err != nil {
		t.Fatalf("recording the proposed PR reading: %v", err)
	}
	scope := l2.ScopeKey(testRepo(src), src, repo, nil)
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
	if history[1].Judgement != l2.JudgementRestates {
		t.Errorf("the merged reading is judged %q, want restates", history[1].Judgement)
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

func TestTheWorkerIsIdempotentAcrossRestarts(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo, nil)
	first := newAsserter(t, pool, src, loadFixtures(t))
	assertDoc(t, first, f.issueID, scope)
	assertDoc(t, first, f.prID, scope)

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

func TestRetryingTheSameVersionWritesNothingTwice(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo, nil)
	a := newAsserter(t, pool, src, loadFixtures(t))
	assertDoc(t, a, f.issueID, scope)

	if _, err := pool.Exec(t.Context(),
		`UPDATE l2_asserted SET distilled_at = distilled_at - interval '1 second' WHERE doc_id = $1`, f.issueID); err != nil {
		t.Fatalf("moving the recorded version: %v", err)
	}

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

func TestARedistilledDocumentReplacesItsOwnStance(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	f := store(t, pool, src)
	scope := l2.ScopeKey(testRepo(src), src, repo, nil)
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
	var s4 *l2.Stance
	for i, st := range after {
		if st.Position == issuePosition && st.ID != s1.ID {
			s4 = &after[i]
		}
	}
	if s4 == nil || s4.Judgement != l2.JudgementChanges || s4.Supersedes != s3.ID {
		t.Errorf("the reading without the comment = %+v, want the issue's proposal again, changing and superseding its restatement %s", s4, s3.ID)
	}

	if _, err := pool.Exec(t.Context(),
		`UPDATE l2_asserted SET distilled_at = distilled_at - interval '1 second' WHERE doc_id = $1`, f.prID); err != nil {
		t.Fatalf("moving the recorded version: %v", err)
	}
	jev := newRuleJev()
	if r := assertDoc(t, newJevAsserter(t, pool, src, loadFixtures(t), jev), f.prID, scope); r.Positions != 1 || r.StancesWritten != 0 {
		t.Errorf("Assert(the pull request again) = %+v, want its one position, already stored", r)
	}
	if compared := jev.asked("same_answer"); len(compared) != 1 || !strings.Contains(compared[0].State, `"Previous":"`+prPosition+`"`) {
		t.Errorf("stance comparisons = %+v, want one against the merged pull request's position", compared)
	}
}

func TestWhatTheModelWroteIsCleanedBeforeItIsStored(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
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

func TestDocumentsOutsideThePipelineAreSkipped(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
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

func TestRunReadsWhatIsEnqueuedAndWhatWasMissed(t *testing.T) {
	pool := dbtest.Scratch(t, "hearsay_assert")
	src := dbtest.UniqueID()
	store(t, pool, src)
	issue, _ := documents(t, src)

	if ok, err := l2.EnqueueAssertion(t.Context(), pool, testRepo(src), issue, repo, nil); err != nil || !ok {
		t.Fatalf("EnqueueAssertion() = %v, %v", ok, err)
	}

	registry, err := llm.NewFake(llm.Default(), loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- assertworker.Run(ctx, testConfig(src), assertworker.Deps{Pool: pool, LLM: registry, Judgments: newRuleJev().judgments(t)})
	}()

	poll.Until(t, "both documents to be read", func() bool {
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

func TestRunRefusesWithoutJev(t *testing.T) {
	pool := dbtest.Pool(t)
	registry, err := llm.NewFake(llm.Default(), llm.NewFixtures())
	if err != nil {
		t.Fatal(err)
	}
	err = assertworker.Run(t.Context(), testConfig(dbtest.UniqueID()), assertworker.Deps{Pool: pool, LLM: registry})
	if err == nil || !strings.Contains(err.Error(), "needs Jev") {
		t.Fatalf("Run() without Jev = %v, want a refusal", err)
	}
}

func TestEnqueueAssertionOnlyForOutcomesThatAssert(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	issue, _ := documents(t, src)
	open, _, err := issue.WithBody(l1.Body{Summary: "Open.", OutcomeKind: l1.OutcomeOpen})
	if err != nil {
		t.Fatalf("WithBody() = %v", err)
	}
	if ok, err := l2.EnqueueAssertion(t.Context(), pool, testRepo(src), open, repo, nil); err != nil || ok {
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
