//go:build integration

package assertworker_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

// assertedTopic opens a topic from the fixture's issue under the source's
// scope, the way the worker would have, and gives it the issue's stance.
func assertedTopic(t *testing.T, pool *pgxpool.Pool, src string) (l2.Topic, l2.Stance) {
	t.Helper()
	f := store(t, pool, src)
	graph := l2.New(pool)
	topic := l2.Topic{ID: l2.TopicID(src, f.issueID, 0, topicName), Scope: src, Name: topicName,
		ACL: connector.ACL{{Kind: connector.ACLPublic}}, OpenedBy: f.issueID}
	if _, err := graph.OpenTopic(t.Context(), topic); err != nil {
		t.Fatalf("OpenTopic() = %v", err)
	}
	issue, _, err := graph.AppendStance(t.Context(), l2.Stance{
		ID: l2.StanceID(topic.ID, f.issueID, issuePosition, at(0), l2.TierInferred), TopicID: topic.ID,
		Position: issuePosition, StatedAt: at(0), Evidence: []string{f.issueID}, Tier: l2.TierInferred,
		ACL: connector.ACL{{Kind: connector.ACLPublic}},
	}, at(0))
	if err != nil {
		t.Fatalf("AppendStance(issue) = %v", err)
	}
	return topic, issue
}

// writeAssertion writes the L0 event the API's assert call writes.
func writeAssertion(t *testing.T, pool *pgxpool.Pool, as l2.Assertion, when time.Time) string {
	t.Helper()
	native, _ := json.Marshal(as)
	ev := connector.Event{
		Source: connector.SelfSource, NativeID: as.NativeID(), Kind: connector.KindAssertion, Time: when,
		Payload: connector.Payload{
			Artifact:  as.NativeID(),
			Container: connector.Container{Kind: connector.ContainerWorkspace, NativeID: "api"},
			Text:      as.Position,
			Author:    &connector.Identity{Source: connector.SelfSource, Kind: connector.IdentityAgent, NativeID: as.Agent},
			Native:    native,
		},
		ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: connector.SelfSource, NativeID: as.Principal}},
	}
	appended, err := l0.New(pool).Append(t.Context(), ev)
	if err != nil {
		t.Fatalf("Append(assertion) = %v", err)
	}
	return appended.ID
}

// The acceptance criterion from the worker's side: an assertion becomes a
// stance on its topic with no model call — the fake here has no answer to
// give — of class agent, by the agent, citing what it cited; and one whose job
// never ran is picked up at startup.
func TestAnAssertionIsAppendedWithNoModelCall(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	topic, issueStance := assertedTopic(t, pool, src)
	f := newFixture(src)
	as := l2.Assertion{Topic: topic.ID, Position: prPosition, Evidence: l2.SortedEvidence([]string{f.prID, f.issueID}), Agent: "shed", Principal: "kyle"}
	id := writeAssertion(t, pool, as, at(5))

	registry, err := llm.NewFake(llm.Default(), llm.NewFixtures())
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- assertworker.Run(ctx, testConfig(src), assertworker.Deps{Pool: pool, LLM: registry}) }()
	graph := l2.New(pool)
	var history []l2.Stance
	waitFor(t, "the assertion's stance", func() bool {
		history, err = graph.StanceHistory(t.Context(), topic.ID)
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

	got := history[1]
	if got.Assertion != id || got.Author != "shed" || got.Position != prPosition || !got.StatedAt.Equal(at(5)) ||
		!slices.Equal(got.Evidence, as.Evidence) || got.Supersedes != issueStance.ID || got.Tier != l2.TierInferred ||
		got.ID != l2.AssertionStanceID(topic.ID, id) || got.Judgement != l2.JudgementUnknown {
		t.Errorf("the asserted stance = %+v, want the agent's position on the topic, citing %v, superseding the issue's", got, as.Evidence)
	}
	// It cites a merged pull request and is still an agent's proposal.
	assessed, err := graph.Assess(t.Context(), config.Authority{}, l1.Reader{}, []l2.Topic{topic})
	if err != nil {
		t.Fatalf("Assess() = %v", err)
	}
	if a := assessed[0]; !a.Stands || a.Standing.Current.ID != issueStance.ID || a.Standing.Tier != l2.TierInferred {
		t.Errorf("the topic stands at %s (%s), want the issue's stance, inferred: an issue outranks an agent", a.Standing.Current.ID, a.Standing.Tier)
	}

	// Appending again, as a retried job would, writes nothing.
	if written, err := assertworker.AppendAssertion(t.Context(), pool, config.Authority{}, id, src); err != nil || written {
		t.Errorf("AppendAssertion(again) = %v, %v, want nothing written", written, err)
	}
	if again, err := graph.StanceHistory(t.Context(), topic.ID); err != nil || len(again) != 2 {
		t.Errorf("StanceHistory() after a retry = %d stances, %v, want 2", len(again), err)
	}
}

func TestAppendAssertionRefusesWhatItCannotSerialize(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	topic, _ := assertedTopic(t, pool, src)
	f := newFixture(src)
	id := writeAssertion(t, pool, l2.Assertion{Topic: topic.ID, Position: "p", Evidence: []string{f.issueID}, Agent: "shed", Principal: "kyle"}, at(5))

	if _, err := assertworker.AppendAssertion(t.Context(), pool, config.Authority{}, id, "another-scope"); err == nil {
		t.Error("AppendAssertion() under another scope's key = nil, want an error: the queue would not be serializing the topic")
	}
	missing := connector.EventID(connector.SelfSource, "assertion:"+src)
	if written, err := assertworker.AppendAssertion(t.Context(), pool, config.Authority{}, missing, src); err != nil || written {
		t.Errorf("AppendAssertion(an event L0 does not hold) = %v, %v, want nothing to do", written, err)
	}
	if !assertworker.IsAssertion(id) || assertworker.IsAssertion(f.issueID) {
		t.Errorf("IsAssertion(%s, %s) is wrong", id, f.issueID)
	}
}
