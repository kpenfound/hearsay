//go:build integration

package deletion_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Deleting the event a gesture came from takes the gesture out of force: a
// ratification no longer ratifies, and a pin is taken out, or handed to the
// next pin in force. The ledger keeps the gesture, which holds ids and no
// content, and the preview names it.
func TestDeletingAGesturesEventTakesItOutOfForce(t *testing.T) {
	pool := scratch(t)
	ctx := t.Context()
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	reaction := func(native, user string) string {
		t.Helper()
		ev := connector.Event{
			Source: "chat", NativeID: native, Kind: connector.KindMessage, Time: when,
			Payload: connector.Payload{
				Artifact: native, Text: "✅",
				Container: connector.Container{Kind: connector.ContainerChannel, NativeID: "team"},
				Author:    &connector.Identity{Source: "chat", Kind: connector.IdentityUser, NativeID: user},
			},
			ACL: connector.ACL{{Kind: connector.ACLPublic}},
		}
		if _, err := l0.New(pool).Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
		return connector.EventID("chat", native)
	}
	ratifyEvent, kylesPinEvent, samsPinEvent := reaction("r1", "u1"), reaction("r2", "u1"), reaction("r3", "u2")
	if _, err := pool.Exec(ctx, `INSERT INTO l1_docs(id,kind,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,acl,text,raw_text,body,outcome_kind,artifact_class,scope)
VALUES('l1:chat:thread','chat_thread','chat','thread',ARRAY['evt:chat:thread'],$1,$1,$1,'[{"kind":"public"}]','text','raw','{}','decided','chat_thread',ARRAY['code:team'])`, when); err != nil {
		t.Fatal(err)
	}
	graph := l2.New(pool)
	const scope = "source:chat"
	topic := l2.Topic{ID: l2.TopicID(scope, "l1:chat:thread", 0, "the lock"), Scope: scope, Name: "the lock",
		ACL: connector.ACL{{Kind: connector.ACLPublic}}, OpenedBy: "l1:chat:thread"}
	if _, err := graph.OpenTopic(ctx, topic); err != nil {
		t.Fatal(err)
	}
	st := l2.Stance{
		ID: l2.StanceID(topic.ID, "l1:chat:thread", "the queue takes the lock", when, l2.TierInferred), TopicID: topic.ID,
		Position: "the queue takes the lock", StatedAt: when, Evidence: []string{"l1:chat:thread"},
		Tier: l2.TierInferred, ACL: topic.ACL,
	}
	if _, _, err := graph.AppendStance(ctx, st, when); err != nil {
		t.Fatal(err)
	}
	repo := config.Repo{Principals: []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: "chat", NativeID: "u1"}}},
		{ID: "sam", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: "chat", NativeID: "u2"}}},
	}}
	record := func(event, by string, action l2.GestureAction) l2.Gesture {
		t.Helper()
		held, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		g, _, err := l2.RecordGesture(held, pool, repo, l2.GestureRequest{Event: event, Principal: by, Action: action, Documents: []string{"l1:chat:thread"}})
		if err != nil {
			t.Fatalf("RecordGesture(%s) = %v", event, err)
		}
		return g
	}
	ratified := func() []string {
		t.Helper()
		c, err := graph.Corrections(ctx, []string{st.ID})
		if err != nil {
			t.Fatal(err)
		}
		return c.Ratified
	}
	pins := func() []l2.Pin {
		t.Helper()
		got, err := graph.Pins(ctx, "code:team")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	ratify := record(ratifyEvent, "kyle", l2.GestureRatify)
	kyles := record(kylesPinEvent, "kyle", l2.GesturePin)
	sams := record(samsPinEvent, "sam", l2.GesturePin)
	if got := ratified(); !slices.Equal(got, []string{st.ID}) {
		t.Fatalf("ratified = %v, want %s", got, st.ID)
	}

	sel := deletion.Selector{Event: kylesPinEvent}
	preview, err := deletion.Walk(ctx, pool, repo, sel, "a mistaken reaction")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preview.Gestures, []int64{kyles.ID}) || preview.Counts()["gestures"] != 1 {
		t.Errorf("the preview names gestures %v, want kyle's pin %d", preview.Gestures, kyles.ID)
	}
	if _, err := deletion.Apply(ctx, pool, repo, sel, "a mistaken reaction", "kyle"); err != nil {
		t.Fatal(err)
	}
	if got := pins(); len(got) != 1 || got[0] != (l2.Pin{Scope: "code:team", L1: "l1:chat:thread", PinnedBy: "sam", PinnedAt: sams.At}) {
		t.Errorf("after deleting kyle's pin's event, Pins = %+v, want sam's pin in its place", got)
	}
	if _, err := deletion.Apply(ctx, pool, repo, deletion.Selector{Event: samsPinEvent}, "a mistaken reaction", "kyle"); err != nil {
		t.Fatal(err)
	}
	if got := pins(); len(got) != 0 {
		t.Errorf("after deleting both pins' events, Pins = %+v, want none", got)
	}

	if _, err := deletion.Apply(ctx, pool, repo, deletion.Selector{Author: "chat:u1"}, "kyle left", "sam"); err != nil {
		t.Fatal(err)
	}
	if got := ratified(); len(got) != 0 {
		t.Errorf("after deleting the ratification's event, ratified = %v, want none", got)
	}
	gs, err := graph.Gestures(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 3 || gs[0].ID != ratify.ID || !slices.Equal(gs[0].Stances, []string{st.ID}) {
		t.Errorf("the ledger = %+v, want every gesture kept", gs)
	}
	// A retry of the deleted event records nothing and brings nothing back.
	if _, recorded, err := l2.RecordGesture(ctx, pool, repo, l2.GestureRequest{Event: ratifyEvent, Principal: "kyle", Action: l2.GestureRatify, Documents: []string{"l1:chat:thread"}}); err != nil || recorded {
		t.Errorf("RecordGesture(the deleted event again) = %v, recorded %v; want nothing recorded", err, recorded)
	}
	if got := ratified(); len(got) != 0 {
		t.Errorf("after a retry, ratified = %v, want none", got)
	}
}
