//go:build integration

package l2_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/queue"
)

// ratifiers is an authority under which kyle and sam may ratify by hand, and
// nobody else.
const ratifiers = "scope: \"*\"\nratified_by:\n  principals: [kyle, sam]\n"

func gestureEvent() string { return connector.EventID("discord", "reaction:"+unique()) }

// gesture records one and fails the test on a refusal.
func gesture(t *testing.T, pool *pgxpool.Pool, repo config.Repo, req l2.GestureRequest) l2.Gesture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	g, recorded, err := l2.RecordGesture(ctx, pool, repo, req)
	if err != nil || !recorded {
		t.Fatalf("RecordGesture(%+v) = %v, recorded %v", req, err, recorded)
	}
	return g
}

func ledger(t *testing.T, store *l2.Store, scope string) []l2.Gesture {
	t.Helper()
	gs, err := store.Gestures(t.Context(), scope)
	if err != nil {
		t.Fatalf("Gestures(%s) = %v", scope, err)
	}
	return gs
}

// standsAt is where a topic stands for a reader who may read everything.
func standsAt(t *testing.T, store *l2.Store, topic l2.Topic) string {
	t.Helper()
	assessed, err := store.Assess(t.Context(), config.Authority{}, l1.Reader{}, []l2.Topic{topic})
	if err != nil {
		t.Fatalf("Assess() = %v", err)
	}
	if !assessed[0].Stands {
		return "nowhere"
	}
	return assessed[0].Standing.Current.Position + " @ " + string(assessed[0].Standing.Tier)
}

// A ratify or a demote applies to every live stance drawn from the documents
// it names, across topics: not to one a later reading of its document retired,
// and not to one an agent asserted citing it. Who may make one, and retrying
// one, write nothing more.
func TestAGestureAppliesToTheLiveStancesDrawnFromItsDocuments(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	scope, elsewhere := unique(), unique()
	lock := namedTopic(t, store, scope, "the lock")
	queueTopic := namedTopic(t, store, scope, "the queue")
	thread, issue := "l1:discord:"+unique(), "l1:github:"+unique()
	retired := addStance(t, store, lock, thread, "the queue takes the lock", 1)
	reread := addStance(t, store, lock, thread, "the queue keeps the lock", 2)
	onQueue := addStance(t, store, queueTopic, issue, "one queue per service", 3)
	event := connector.EventID(connector.SelfSource, "assertion:"+unique())
	asserted := stance(lock, thread, "the agent takes the lock", 4)
	asserted.ID, asserted.Assertion, asserted.Author = l2.AssertionStanceID(lock.ID, event), event, "shed"
	if _, _, err := store.AppendStance(ctx, asserted, asserted.StatedAt); err != nil {
		t.Fatal(err)
	}
	other := namedTopic(t, store, elsewhere, "the lock")
	otherDoc := "l1:discord:" + unique()
	addStance(t, store, other, otherDoc, "the lock is global", 1)

	req := l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureRatify, Documents: []string{issue, thread}}
	g := gesture(t, pool, repo, req)
	if want := slices.Sorted(slices.Values([]string{reread.ID, onQueue.ID})); g.Scope != scope || !slices.Equal(g.Stances, want) ||
		!slices.Equal(g.Documents, slices.Sorted(slices.Values([]string{issue, thread}))) || g.Principal != "kyle" || g.At.IsZero() {
		t.Fatalf("the ratify = %+v, want it on %v in %s, not on the retired %s or the asserted %s", g, want, scope, retired.ID, asserted.ID)
	}

	// A retry of the same event records nothing and returns the gesture.
	again, recorded, err := l2.RecordGesture(ctx, pool, repo, req)
	if err != nil || recorded || again.ID != g.ID {
		t.Errorf("RecordGesture(again) = %+v, %v, recorded %v; want gesture %d back and nothing recorded", again, err, recorded, g.ID)
	}
	// An event is one gesture: another from it is refused.
	changed := req
	changed.Action = l2.GestureDemote
	if _, _, err := l2.RecordGesture(ctx, pool, repo, changed); !errors.Is(err, l2.ErrInvalid) {
		t.Errorf("RecordGesture(a demote from the ratify's event) = %v, want ErrInvalid", err)
	}

	refused := []struct {
		name string
		req  l2.GestureRequest
		want error
	}{
		{"a principal nobody configured", l2.GestureRequest{Principal: "stranger", Action: l2.GestureDemote, Documents: []string{thread}}, l2.ErrNotAllowed},
		{"an agent", l2.GestureRequest{Principal: "bot", Action: l2.GestureDemote, Documents: []string{thread}}, l2.ErrNotAllowed},
		{"nobody", l2.GestureRequest{Action: l2.GestureDemote, Documents: []string{thread}}, l2.ErrNotAllowed},
		{"documents in two scopes", l2.GestureRequest{Principal: "kyle", Action: l2.GestureDemote, Documents: []string{thread, otherDoc}}, l2.ErrInvalid},
		{"a document no live stance is drawn from", l2.GestureRequest{Principal: "kyle", Action: l2.GestureDemote, Documents: []string{"l1:discord:" + unique()}}, l2.ErrNotFound},
		{"an undo of an event no gesture came from", l2.GestureRequest{Principal: "kyle", Action: l2.GestureUndo, Undoes: gestureEvent()}, l2.ErrNotFound},
	}
	for _, tt := range refused {
		tt.req.Event = gestureEvent()
		if _, _, err := l2.RecordGesture(ctx, pool, repo, tt.req); !errors.Is(err, tt.want) {
			t.Errorf("RecordGesture(%s) = %v, want %v", tt.name, err, tt.want)
		}
	}
	restricted := loadRepo(t, "scope: \"*\"\nratified_by:\n  principals: [kyle]\n")
	if _, _, err := l2.RecordGesture(ctx, pool, restricted, l2.GestureRequest{Event: gestureEvent(), Principal: "sam", Action: l2.GestureDemote, Documents: []string{thread}}); !errors.Is(err, l2.ErrNotAllowed) {
		t.Errorf("RecordGesture(by a human ratified_by leaves out) = %v, want ErrNotAllowed", err)
	}
	if got := ledger(t, store, scope); len(got) != 1 || got[0].ID != g.ID {
		t.Errorf("the ledger = %+v, want only the ratify: a refusal writes nothing", got)
	}
	if got := ledger(t, store, elsewhere); len(got) != 0 {
		t.Errorf("the other scope's ledger = %+v, want it empty", got)
	}

	// A caller running under a serial key records only what is in its scope.
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := l2.New(tx).ApplyGesture(ctx, repo, elsewhere, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureDemote, Documents: []string{thread}})
		return err
	})
	if !errors.Is(err, l2.ErrInvalid) {
		t.Errorf("ApplyGesture(under another scope's key) = %v, want ErrInvalid", err)
	}
}

// Ratified and demoted are what a read serves, until an undo or a newer
// stance; an undo is a row of its own, and nothing earlier in the ledger
// changes.
func TestGesturesDecideWhereATopicStands(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	scope := unique()
	topic := namedTopic(t, store, scope, "the lock")
	first, second := "l1:discord:"+unique(), "l1:discord:"+unique()
	addStance(t, store, topic, first, "the queue takes the lock", 1)
	current := stance(topic, second, "the queue keeps the lock", 2)
	current.Judgement = l2.JudgementRestates
	if _, _, err := store.AppendStance(ctx, current, current.StatedAt); err != nil {
		t.Fatal(err)
	}
	if got := standsAt(t, store, topic); got != "the queue keeps the lock @ inferred" {
		t.Fatalf("before any gesture the topic stands at %q", got)
	}

	on := []string{second}
	demote := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureDemote, Documents: on})
	if got := standsAt(t, store, topic); got != "the queue keeps the lock @ contested" {
		t.Errorf("demoted, the topic stands at %q, want contested", got)
	}
	ratify := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "sam", Action: l2.GestureRatify, Documents: on})
	if got := standsAt(t, store, topic); got != "the queue keeps the lock @ ratified" {
		t.Errorf("ratified after the demotion, the topic stands at %q, want ratified", got)
	}
	before := ledger(t, store, scope)

	undoRatify := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: ratify.Event})
	if undoRatify.Undoes != ratify.ID || !slices.Equal(undoRatify.Stances, ratify.Stances) || !slices.Equal(undoRatify.Documents, ratify.Documents) {
		t.Errorf("the undo = %+v, want it to repeat what gesture %d covered", undoRatify, ratify.ID)
	}
	if got := standsAt(t, store, topic); got != "the queue keeps the lock @ contested" {
		t.Errorf("the ratification undone, the topic stands at %q, want the demotion back", got)
	}
	gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: demote.Event})
	if got := standsAt(t, store, topic); got != "the queue keeps the lock @ inferred" {
		t.Errorf("both undone, the topic stands at %q, want inferred", got)
	}

	for _, tt := range []struct {
		name   string
		undoes string
	}{
		{"an undo of a gesture already undone", ratify.Event},
		{"an undo of an undo", undoRatify.Event},
	} {
		if _, _, err := l2.RecordGesture(ctx, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: tt.undoes}); !errors.Is(err, l2.ErrInvalid) {
			t.Errorf("RecordGesture(%s) = %v, want ErrInvalid", tt.name, err)
		}
	}

	// The ledger only grew: the gestures an undo reversed are as they were,
	// and say which undo reversed them.
	after := ledger(t, store, scope)
	if len(after) != 4 {
		t.Fatalf("the ledger has %d gestures, want 4", len(after))
	}
	for i, g := range before {
		was, is := g, after[i]
		was.UndoneBy, is.UndoneBy = 0, 0
		if was.ID != is.ID || was.Event != is.Event || was.Action != is.Action || was.Principal != is.Principal ||
			!slices.Equal(was.Stances, is.Stances) || !was.At.Equal(is.At) {
			t.Errorf("gesture %d changed: %+v, then %+v", g.ID, g, after[i])
		}
	}
	if after[0].UndoneBy != after[3].ID || after[1].UndoneBy != after[2].ID || after[2].UndoneBy != 0 {
		t.Errorf("the ledger = %+v, want each gesture to name its undo", after)
	}

	// A newer stance the topic stands at is not what was demoted.
	gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureDemote, Documents: on})
	if got := standsAt(t, store, topic); got != "the queue keeps the lock @ contested" {
		t.Errorf("demoted again, the topic stands at %q, want contested", got)
	}
	third := "l1:discord:" + unique()
	addStance(t, store, topic, third, "the worker keeps the lock", 3)
	if got := standsAt(t, store, topic); got != "the worker keeps the lock @ inferred" {
		t.Errorf("with a newer stance, the topic stands at %q, want it inferred", got)
	}
}

// A pin writes l2_pins with the pinner and the gesture's time; an undo takes
// it out, or hands it to the next pin in force, and never touches a pin no
// gesture made.
func TestPinGesturesPinAndUnpin(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	src := "s" + unique()
	entity, other := "code:acme/"+src, "tracker:"+src+":acme/api#1"
	put := func(native string, scope ...string) string {
		t.Helper()
		when := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
		doc := l1.Document{
			ID: l1.DocID(src, native), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
			Source: l1.Source{System: src, NativeID: native}, Scope: scope,
			L0Refs: []string{connector.EventID(src, native)}, Time: l1.Times{Created: when, Updated: when, LastActivity: when},
			ACL: public, Text: "text of " + native, RawText: "raw " + native,
			Body: l1.Body{Summary: "s", OutcomeKind: l1.OutcomeDecided},
		}
		if _, err := l1.New(pool).Put(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return doc.ID
	}
	pinsOn := func(scope string) []l2.Pin {
		t.Helper()
		pins, err := store.Pins(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		return pins
	}
	design := put("design", entity, other)

	kyles := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GesturePin, Documents: []string{design}})
	if kyles.Scope != "source:"+src || len(kyles.Pins) != 2 {
		t.Fatalf("the pin = %+v, want it in source:%s, on both entities", kyles, src)
	}
	for _, e := range []string{entity, other} {
		if got := pinsOn(e); len(got) != 1 || got[0] != (l2.Pin{Scope: e, L1: design, PinnedBy: "kyle", PinnedAt: kyles.At}) {
			t.Errorf("Pins(%s) = %+v, want kyle's, at the gesture's time %s", e, got, kyles.At)
		}
	}
	sams := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "sam", Action: l2.GesturePin, Documents: []string{design}})
	if got := pinsOn(entity); len(got) != 1 || got[0].PinnedBy != "kyle" {
		t.Errorf("pinned again, Pins = %+v, want the first pinner kept", got)
	}
	gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: kyles.Event})
	if got := pinsOn(entity); len(got) != 1 || got[0] != (l2.Pin{Scope: entity, L1: design, PinnedBy: "sam", PinnedAt: sams.At}) {
		t.Errorf("kyle's pin undone, Pins = %+v, want sam's, which is still in force", got)
	}
	gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "sam", Action: l2.GestureUndo, Undoes: sams.Event})
	if got := pinsOn(entity); len(got) != 0 {
		t.Errorf("both undone, Pins = %+v, want none", got)
	}

	// A pin nobody made by gesture is not a gesture's to take out.
	spec := put("spec", entity)
	by := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.Pin(ctx, l2.Pin{Scope: entity, L1: spec, PinnedBy: "ops", PinnedAt: by}); err != nil {
		t.Fatal(err)
	}
	pin := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GesturePin, Documents: []string{spec}})
	gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: pin.Event})
	if got := pinsOn(entity); len(got) != 1 || got[0] != (l2.Pin{Scope: entity, L1: spec, PinnedBy: "ops", PinnedAt: by}) {
		t.Errorf("Pins = %+v, want the pin ops made", got)
	}

	for _, tt := range []struct {
		name string
		docs []string
		want error
	}{
		{"a document about no entity", []string{put("chatter")}, l2.ErrInvalid},
		{"a document L1 does not hold", []string{l1.DocID(src, "gone")}, l2.ErrNotFound},
	} {
		if _, _, err := l2.RecordGesture(ctx, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GesturePin, Documents: tt.docs}); !errors.Is(err, tt.want) {
			t.Errorf("RecordGesture(pin %s) = %v, want %v", tt.name, err, tt.want)
		}
	}
}

// A gesture takes its turn among the assert jobs of its scope: it waits for
// the one running and applies to what that job wrote.
func TestGesturesSerializeWithAssertJobs(t *testing.T) {
	pool := scratchPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	scope := unique()
	lock := namedTopic(t, store, scope, "the lock")
	queueTopic := namedTopic(t, store, scope, "the queue")
	thread := "l1:discord:" + unique()
	first := addStance(t, store, lock, thread, "the queue takes the lock", 1)

	client, err := queue.New(pool, queue.Config{Kind: l2.AssertKind(), Lease: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Enqueue(ctx, pool, queue.Request{Kind: l2.AssertKind(), TargetID: thread, SerialKey: scope}); err != nil {
		t.Fatal(err)
	}
	running, err := client.Claim(ctx)
	if err != nil || len(running) != 1 {
		t.Fatalf("Claim() = %v, %v, want the job", running, err)
	}
	type result struct {
		g   l2.Gesture
		err error
	}
	done := make(chan result, 1)
	waiting, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	go func() {
		g, _, err := l2.RecordGesture(waiting, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureRatify, Documents: []string{thread}})
		done <- result{g, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("RecordGesture() = %+v, %v while an assert job runs on its scope, want it to wait", r.g, r.err)
	case <-time.After(4 * queue.HoldPoll):
	}
	second := addStance(t, store, queueTopic, thread, "one queue per service", 2)
	if ok, err := client.Complete(ctx, running[0]); err != nil || !ok {
		t.Fatalf("Complete() = %v, %v", ok, err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("RecordGesture() = %v", r.err)
	}
	if want := slices.Sorted(slices.Values([]string{first.ID, second.ID})); !slices.Equal(r.g.Stances, want) {
		t.Errorf("the ratify covers %v, want %v: the stance the job wrote before it finished", r.g.Stances, want)
	}
}
