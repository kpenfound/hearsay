//go:build integration

package assertworker_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

const (
	otherName     = "How the engine orders its writes"
	otherPosition = "Writes are ordered by the job queue."
)

func twoTopics(t *testing.T, pool *pgxpool.Pool, src string) (fixture, l2.Topic, l2.Topic) {
	t.Helper()
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	graph := l2.New(pool)
	topics, err := graph.Topics(t.Context(), src)
	if err != nil || len(topics) != 1 {
		t.Fatalf("Topics() = %+v, %v, want the issue's", topics, err)
	}
	other := l2.Topic{ID: l2.TopicID(src, f.issueID, 7, otherName), Scope: src, Name: otherName,
		JoinKeys: []string{"item:" + repo + "#12"}, ACL: connector.ACL{{Kind: connector.ACLPublic}}, OpenedBy: f.issueID}
	if _, err := graph.OpenTopic(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	stated := at(1)
	if _, _, err := graph.AppendStance(t.Context(), l2.Stance{
		ID: l2.StanceID(other.ID, f.issueID, otherPosition, stated, l2.TierInferred), TopicID: other.ID, Position: otherPosition,
		StatedAt: stated, Evidence: []string{f.issueID}, Tier: l2.TierInferred, ACL: other.ACL,
	}, stated); err != nil {
		t.Fatal(err)
	}
	return f, topics[0], other
}

func TestJevDecidesWhereEachPositionGoes(t *testing.T) {
	byName := func(asked, other float64) func(string, string, shownCandidate) float64 {
		return func(_, _ string, c shownCandidate) float64 {
			if c.Name == otherName {
				return other
			}
			return asked
		}
	}
	always := func(p float64) func(string, string) float64 { return func(string, string) float64 { return p } }
	for _, tc := range []struct {
		name         string
		sameQuestion func(string, string, shownCandidate) float64
		sameAnswer   func(string, string) float64

		want      string
		judgement l2.Judgement
	}{
		{"the most likely confident yes wins", byName(0.85, 0.95), always(0.04), "other", l2.JudgementChanges},
		{"a tie goes to the first offered", byName(0.9, 0.9), always(0.04), "asked", l2.JudgementChanges},
		{"a yes below the threshold is a no", byName(0.79, 0.7), always(0.96), "new", l2.JudgementUnknown},
		{"a confident no to every candidate opens a topic", byName(0.04, 0.04), always(0.96), "new", l2.JudgementUnknown},
		{"a confident yes to the same answer restates", byName(0.96, 0.04), always(0.81), "asked", l2.JudgementRestates},
		{"a yes to the same answer below the threshold changes", byName(0.96, 0.04), always(0.79), "asked", l2.JudgementChanges},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newPool(t)
			src := newSource(t)
			f, asked, other := twoTopics(t, pool, src)
			jev := newRuleJev()
			jev.sameQuestion, jev.sameAnswer = tc.sameQuestion, tc.sameAnswer
			r := assertDoc(t, newJevAsserter(t, pool, src, loadFixtures(t), jev), f.prID, src)
			written, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			if err != nil || len(written) != 1 {
				t.Fatalf("StancesFrom(pull request) = %+v, %v, want one", written, err)
			}
			got := map[string]string{asked.ID: "asked", other.ID: "other"}[written[0].TopicID]
			if got == "" {
				got = "new"
			}
			if got != tc.want || written[0].Judgement != tc.judgement || (r.TopicsOpened == 1) != (tc.want == "new") {
				t.Errorf("the position is on %s judged %q (%+v), want %s judged %q", got, written[0].Judgement, r, tc.want, tc.judgement)
			}
			if compared := len(jev.asked("same_answer")); (compared == 1) != (tc.want != "new") {
				t.Errorf("Jev was asked %d stance comparisons", compared)
			}
		})
	}
}

func TestATopicOnePositionTookIsNotOfferedToTheNext(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f, asked, other := twoTopics(t, pool, src)
	fx := llm.NewFixtures()
	if err := fx.Add(llm.CompletionFixture{
		Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), assertBudget()),
		Response: llm.Response{JSON: []byte(`{"assertions":[{"topic_name":"` + topicName + `","position":"` + prPosition + `"},` +
			`{"topic_name":"` + topicName + `","position":"The lock is held for the whole write."}]}`), StopReason: llm.StopEnd, Model: "recorded"},
	}); err != nil {
		t.Fatal(err)
	}
	jev := newRuleJev()
	r := assertDoc(t, newJevAsserter(t, pool, src, fx, jev), f.prID, src)
	if r.Positions != 2 || r.TopicsOpened != 1 {
		t.Fatalf("Assert(pull request) = %+v, want two positions, the second on a topic of its own", r)
	}
	asks := jev.asked("same_question")
	if len(asks) != 2 {
		t.Fatalf("Jev was asked %d topic questions, want one per position", len(asks))
	}
	var second []string
	for _, c := range shown(t, asks[1]) {
		second = append(second, c.Name)
	}
	if !slices.Equal(second, []string{otherName}) {
		t.Errorf("the second position was offered %q, want only the topic the first did not take", second)
	}
	written, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
	if err != nil || len(written) != 2 {
		t.Fatalf("StancesFrom(pull request) = %+v, %v", written, err)
	}
	topics := map[string]bool{}
	for _, st := range written {
		topics[st.TopicID] = true
	}
	if len(topics) != 2 || !topics[asked.ID] || topics[other.ID] {
		t.Errorf("the positions are on %v, want the issue's topic and a new one", topics)
	}
}

func TestAJudgmentThatCannotBeMadeFailsTheJob(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, pool *pgxpool.Pool, src string) *assertworker.Asserter
		check func(t *testing.T, err error)
	}{
		{
			name: "Jev fails",
			build: func(t *testing.T, pool *pgxpool.Pool, src string) *assertworker.Asserter {
				jev := newRuleJev()
				jev.fail = &llm.Error{Status: 503, Retryable: true, Msg: "provider error text"}
				return newJevAsserter(t, pool, src, loadFixtures(t), jev)
			},
			check: func(t *testing.T, err error) {
				var je *llm.JudgmentError
				if !errors.As(err, &je) || je.Failure != llm.JudgmentUnavailable || strings.Contains(err.Error(), "provider error text") {
					t.Fatalf("Assert() = %v, want Jev unavailable, sanitized", err)
				}
			},
		},
		{
			name: "no Jev",
			build: func(t *testing.T, pool *pgxpool.Pool, src string) *assertworker.Asserter {
				registry, err := llm.NewFake(llm.Default(), loadFixtures(t))
				if err != nil {
					t.Fatal(err)
				}
				a, err := assertworker.New(pool, registry, testConfig(src))
				if err != nil {
					t.Fatal(err)
				}
				return a
			},
			check: func(t *testing.T, err error) {
				if err == nil || !strings.Contains(err.Error(), "needs Jev") {
					t.Fatalf("Assert() = %v, want Jev required", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newPool(t)
			src := newSource(t)
			f := store(t, pool, src)
			a := tc.build(t, pool, src)

			if r := assertDoc(t, a, f.issueID, src); r.TopicsOpened != 1 {
				t.Fatalf("Assert(issue) = %+v, want a topic opened", r)
			}
			_, err := a.Assert(t.Context(), f.prID, src)
			tc.check(t, err)
			if written, err := l2.New(pool).StancesFrom(t.Context(), f.prID); err != nil || len(written) != 0 {
				t.Fatalf("StancesFrom(pull request) = %+v, %v, want nothing written", written, err)
			}
			assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.prID, src)
			writtenJudgment(t, pool, f.prID, l2.JudgementChanges)
		})
	}
}

func TestAJournalRefusalFailsTheJob(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	a := newAsserter(t, pool, src, loadFixtures(t))
	a.WithJournalCommit(func(ctx context.Context, tx pgx.Tx, rs []l1.JudgmentRecord, obs []l1.PipelineObservation, opts l1.JournalCommitOptions) error {
		if len(rs) > 0 {
			return l1.JournalInputsChanged
		}
		return l1.CommitJudgments(ctx, tx, rs, obs, opts)
	})
	if _, err := a.Assert(t.Context(), f.prID, src); !errors.Is(err, l1.JournalInputsChanged) {
		t.Fatalf("Assert() with a refusing journal = %v, want the refusal", err)
	}
	if written, err := l2.New(pool).StancesFrom(t.Context(), f.prID); err != nil || len(written) != 0 {
		t.Fatalf("StancesFrom(pull request) = %+v, %v, want nothing written", written, err)
	}
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.prID, src)
	writtenJudgment(t, pool, f.prID, l2.JudgementChanges)
}

func TestCommittedJudgmentsAreReusedAcrossARestart(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	var topicsBefore []byte
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(t)) FROM l2_topics t`).Scan(&topicsBefore); err != nil {
		t.Fatal(err)
	}
	jev := newRuleJev()
	jev.sameAnswer = func(string, string) float64 { return 0.96 }
	a := newJevAsserter(t, pool, src, loadFixtures(t), jev)
	committed := map[string]string{}
	a.WithJournalCommit(func(ctx context.Context, tx pgx.Tx, rs []l1.JudgmentRecord, obs []l1.PipelineObservation, opts l1.JournalCommitOptions) error {
		if len(rs) == 0 {
			return l1.CommitJudgments(ctx, tx, rs, obs, opts)
		}
		for _, r := range rs {
			if r.Mode != "accepted" || r.Policy != "threshold" || r.Outcome != "answer" || r.ModelVersion != jevModel {
				t.Fatalf("judgment = %+v", r)
			}
			committed[r.Task] = r.ID
		}

		if _, err := pool.Exec(ctx, `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl)
     VALUES($1,$2,'unrelated','issue','unrelated',now(),'{"artifact":"unrelated","title":"Separate issue","text":"Separate discussion."}','[{"kind":"public"}]')`, "evt:"+src+":unrelated", src); err != nil {
			t.Fatal(err)
		}
		return l1.CommitJudgments(ctx, tx, rs, obs, opts)
	})
	assertDoc(t, a, f.prID, src)
	writtenJudgment(t, pool, f.prID, l2.JudgementRestates)
	if jev.calls() != 2 || len(committed) != 2 {
		t.Fatalf("Jev calls = %d, committed = %v, want one topic and one stance judgment", jev.calls(), committed)
	}

	for _, sql := range []string{`DELETE FROM l2_stances WHERE $1=ANY(evidence)`, `DELETE FROM l2_asserted WHERE doc_id=$1`} {
		if _, err := pool.Exec(t.Context(), sql, f.prID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE l2_topics t SET about=b.about,join_keys=b.join_keys
      FROM jsonb_populate_recordset(NULL::l2_topics,$1) b WHERE t.id=b.id`, topicsBefore); err != nil {
		t.Fatal(err)
	}
	again := newRuleJev()
	restarted := newJevAsserter(t, pool, src, loadFixtures(t), again)
	restarted.WithJournalCommit(func(ctx context.Context, tx pgx.Tx, rs []l1.JudgmentRecord, obs []l1.PipelineObservation, opts l1.JournalCommitOptions) error {
		for _, o := range obs {
			if o.JudgmentRequests == nil || *o.JudgmentRequests != 0 || o.JudgmentAttempts == nil || *o.JudgmentAttempts != 0 {
				t.Fatalf("reused judgments counted again: %+v", o)
			}
		}
		for _, r := range rs {
			if r.ID != committed[r.Task] {
				t.Fatalf("the restart did not reuse the committed %s judgment: %+v", r.Task, r)
			}
		}
		return l1.CommitJudgments(ctx, tx, rs, obs, opts)
	})
	assertDoc(t, restarted, f.prID, src)

	writtenJudgment(t, pool, f.prID, l2.JudgementRestates)
	if again.calls() != 0 {
		t.Fatalf("the restart asked Jev %d times, want none", again.calls())
	}
	for task, id := range committed {
		records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), task)
		if err != nil || len(records) != 1 || records[0].ID != id || records[0].State != "committed" {
			t.Fatalf("task=%s records=%+v %v", task, records, err)
		}
	}
}

func TestHumanGesturesDetermineStandingAfterJudgment(t *testing.T) {
	for _, choice := range []l2.Judgement{l2.JudgementChanges, l2.JudgementRestates} {
		t.Run(string(choice), func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f := store(t, pool, src)
			assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
			jev := newRuleJev()
			jev.sameAnswer = func(string, string) float64 { return likely(choice == l2.JudgementRestates) }
			assertDoc(t, newJevAsserter(t, pool, src, loadFixtures(t), jev), f.prID, src)
			writtenJudgment(t, pool, f.prID, choice)
			calls := jev.calls()
			graph := l2.New(pool)
			topics, err := graph.Topics(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			for i, action := range []l2.GestureAction{l2.GestureDemote, l2.GestureRatify} {
				ev := event(src, connector.KindCommand, fmt.Sprintf("gesture-%d", i), at(10+i), "kpenfound", "u1", "gesture", "gesture")
				stored, err := l0.New(pool).Append(t.Context(), ev)
				if err != nil {
					t.Fatal(err)
				}
				req := l2.GestureRequest{Event: stored.ID, Principal: "kyle", Action: action, Documents: []string{f.prID}}
				if err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
					_, _, e := l2.New(tx).ApplyGesture(t.Context(), testRepo(src), src, req)
					return e
				}); err != nil {
					t.Fatal(err)
				}
				assessed, err := graph.Assess(t.Context(), testRepo(src).Authority, judgmentReader(), topics)
				if err != nil || len(assessed) != 1 {
					t.Fatalf("Assess = %+v, %v", assessed, err)
				}
				want := l2.TierContested
				if action == l2.GestureRatify {
					want = l2.TierRatified
				}
				if assessed[0].Standing.Tier != want {
					t.Fatalf("after %s: %s, want %s", action, assessed[0].Standing.Tier, want)
				}
				if jev.calls() != calls {
					t.Fatal("a gesture or a standing asked Jev")
				}
			}
		})
	}
}

func TestAssertHandlerRecordsKnownQueueDelay(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	a := newAsserter(t, pool, src, loadFixtures(t))
	job := queue.Job{TargetID: f.issueID, SerialKey: src, EnqueuedAt: at(0), StartedAt: at(0).Add(3 * time.Second)}
	if err := a.Handle(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	observations, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
	if err != nil || len(observations) != 1 {
		t.Fatalf("observations = %+v, %v", observations, err)
	}
	observation := observations[0]
	if observation.QueueDelayNS == nil || *observation.QueueDelayNS != int64(3*time.Second) || observation.Cohort != "accepted" || observation.HandlerNS == nil || *observation.HandlerNS <= 0 {
		t.Fatalf("stage timing = %+v", observation)
	}
}
