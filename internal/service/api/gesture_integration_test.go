//go:build integration

package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// A person's ratify and demote are what the bundle, the L3 view and
// stance_history serve, until an undo or a newer stance.
func TestGesturesAreServedByEveryRead(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	graph := l2.New(w.pool)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	first := w.putIn(w.scope, w.project+"#30", l1.KindIssue, 10, "The platform team runs the migration.", public)
	second := w.putIn(w.scope, w.project+"#31", l1.KindIssue, 11, "The platform team runs it, from a runbook.", public)
	third := w.putIn(w.scope, w.project+"#32", l1.KindIssue, 12, "Each service team runs its migration.", public)
	topic := l2.Topic{ID: l2.TopicID(w.src, first, 0, "who runs the migration"), Scope: w.src, Name: "who runs the migration",
		About: []string{w.scope}, ACL: public, OpenedBy: first}
	if _, err := graph.OpenTopic(ctx, topic); err != nil {
		t.Fatal(err)
	}
	stance := func(doc, position string, hour int, judgement l2.Judgement) {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		if _, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(topic.ID, doc, position, at, l2.TierInferred), TopicID: topic.ID, Position: position, Author: "kyle",
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, Judgement: judgement, ACL: public,
		}, at); err != nil {
			t.Fatal(err)
		}
	}
	gesture := func(by string, action l2.GestureAction, undoes string) l2.Gesture {
		t.Helper()
		held, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req := l2.GestureRequest{Event: connector.EventID("discord", "reaction:"+newSource()), Principal: by, Action: action, Undoes: undoes}
		if action != l2.GestureUndo {
			req.Documents = []string{second}
		}
		g, _, err := l2.RecordGesture(held, w.pool, repo(w.src), req)
		if err != nil {
			t.Fatalf("RecordGesture(%+v) = %v", req, err)
		}
		return g
	}
	var none config.Authority
	same := func(step string, want string) {
		t.Helper()
		got := w.standingOf(t, w.calls, none, topic)
		if got != (standing{want, want, want}) {
			t.Errorf("%s: bundle %q, L3 %q, history %q; want %q from all three", step, got.bundle, got.view, got.history, want)
		}
	}

	stance(first, "the platform team", 10, l2.JudgementUnknown)
	stance(second, "the platform team, from a runbook", 11, l2.JudgementRestates)
	same("two issues agree", "the platform team, from a runbook @ inferred")
	demoted := gesture("kyle", l2.GestureDemote, "")
	same("kyle demotes it", "the platform team, from a runbook @ contested")
	ratified := gesture("sam", l2.GestureRatify, "")
	same("sam ratifies it", "the platform team, from a runbook @ ratified")
	gesture("sam", l2.GestureUndo, ratified.Event)
	same("sam's ratification undone", "the platform team, from a runbook @ contested")
	stance(third, "each service team", 12, l2.JudgementRestates)
	same("a newer stance", "each service team @ inferred")
	gesture("kyle", l2.GestureUndo, demoted.Event)
	same("the demotion undone", "each service team @ inferred")
}
