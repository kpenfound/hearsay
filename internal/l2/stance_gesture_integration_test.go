//go:build integration

package l2_test

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l2"
)

type stanceFixture struct {
	scope, thread string
	lock, queue   l2.Topic
	onLock, onQ   l2.Stance
}

func newStanceFixture(t *testing.T, store *l2.Store) stanceFixture {
	t.Helper()
	f := stanceFixture{scope: unique(), thread: "l1:discord:" + unique()}
	f.lock = namedTopic(t, store, f.scope, "the lock")
	f.queue = namedTopic(t, store, f.scope, "the queue")
	f.onLock = addStance(t, store, f.lock, f.thread, "the queue keeps the lock", 1)
	f.onQ = addStance(t, store, f.queue, f.thread, "one queue per service", 2)
	return f
}

func stanceGesture(event, who string, action l2.GestureAction, stance string) l2.GestureRequest {
	return l2.GestureRequest{Event: event, Principal: who, Action: action, Stance: stance}
}

func TestAStanceGestureAppliesToTheNamedStanceAlone(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	f := newStanceFixture(t, store)

	req := stanceGesture(gestureEvent(), "kyle", l2.GestureRatify, f.onLock.ID)
	req.Carrier = "bot"
	g := gesture(t, pool, repo, req)
	if g.Scope != f.scope || g.Principal != "kyle" || g.Carrier != "bot" || g.Stance != f.onLock.ID ||
		!slices.Equal(g.Stances, []string{f.onLock.ID}) || !slices.Equal(g.Documents, []string{f.thread}) {
		t.Fatalf("the ratify = %+v, want kyle's, carried by bot, on %s alone", g, f.onLock.ID)
	}
	if got := standsAt(t, store, f.lock); got != "the queue keeps the lock @ ratified" {
		t.Errorf("the ratified topic stands at %q", got)
	}
	if got := standsAt(t, store, f.queue); got != "one queue per service @ inferred" {
		t.Errorf("the other stance from the same thread stands at %q, want it untouched", got)
	}

	demote := stanceGesture(gestureEvent(), "sam", l2.GestureDemote, f.onQ.ID)
	d := gesture(t, pool, repo, demote)
	if d.Carrier != "" || !slices.Equal(d.Stances, []string{f.onQ.ID}) {
		t.Fatalf("the demote = %+v, want no carrier and one stance", d)
	}
	if got := standsAt(t, store, f.queue); got != "one queue per service @ contested" {
		t.Errorf("the demoted topic stands at %q", got)
	}
	if got := standsAt(t, store, f.lock); got != "the queue keeps the lock @ ratified" {
		t.Errorf("the ratified topic stands at %q after another's demotion", got)
	}

	read, err := store.GestureByEvent(t.Context(), req.Event)
	if err != nil || read.Carrier != "bot" || read.Stance != f.onLock.ID {
		t.Errorf("GestureByEvent = %+v, %v, want the carrier and stance read back", read, err)
	}
	for _, g := range ledger(t, store, f.scope) {
		if g.ID == d.ID && g.Carrier != "" {
			t.Errorf("a gesture recorded without a carrier lists %q", g.Carrier)
		}
	}

	undo := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: d.Event, DecisionsOnly: true, Carrier: "bot"})
	if undo.Carrier != "bot" || !slices.Equal(undo.Stances, []string{f.onQ.ID}) || undo.UndoesEvent != d.Event {
		t.Errorf("the undo = %+v", undo)
	}
	if got := standsAt(t, store, f.queue); got != "one queue per service @ inferred" {
		t.Errorf("after the undo the topic stands at %q", got)
	}
}

func TestAStanceGestureMayNameAnAssertedStance(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	f := newStanceFixture(t, store)
	event := connector.EventID(connector.SelfSource, "assertion:"+unique())
	asserted := stance(f.lock, f.thread, "the agent takes the lock", 4)
	asserted.ID, asserted.Assertion, asserted.Author = l2.AssertionStanceID(f.lock.ID, event), event, "shed"
	if _, _, err := store.AppendStance(t.Context(), asserted, asserted.StatedAt); err != nil {
		t.Fatal(err)
	}
	g := gesture(t, pool, repo, stanceGesture(gestureEvent(), "kyle", l2.GestureRatify, asserted.ID))
	if !slices.Equal(g.Stances, []string{asserted.ID}) {
		t.Fatalf("the ratify = %+v, want the asserted stance", g)
	}
	if got := standsAt(t, store, f.lock); got != "the agent takes the lock @ ratified" {
		t.Errorf("the topic stands at %q", got)
	}
}

func TestAStanceGestureRefusesWhatIsNotLive(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	f := newStanceFixture(t, store)
	retired := f.onLock
	f.onLock = addStance(t, store, f.lock, f.thread, "the lock moves to the broker", 3)
	withdrawn := addStance(t, store, f.queue, "l1:github:"+unique(), "a queue per team", 4)
	if _, err := pool.Exec(ctx, `UPDATE l2_stances SET withdrawn = true WHERE id = $1`, withdrawn.ID); err != nil {
		t.Fatal(err)
	}
	addStance(t, store, f.queue, f.thread, "one queue per team", 5)
	only := loadRepo(t, "scope: \"*\"\nratified_by:\n  principals: [kyle]\n")
	carrierNamed := loadRepo(t, "scope: \"*\"\nratified_by:\n  principals: [kyle, bot]\n")

	for _, tt := range []struct {
		name  string
		repo  config.Repo
		req   l2.GestureRequest
		want  error
		other []error
	}{
		{"a retired stance", repo, stanceGesture("", "kyle", l2.GestureRatify, retired.ID), l2.ErrNotLive, []error{l2.ErrNotFound, l2.ErrNotAllowed}},
		{"a retired stance, demoted", repo, stanceGesture("", "kyle", l2.GestureDemote, f.onQ.ID), l2.ErrNotLive, []error{l2.ErrNotFound, l2.ErrNotAllowed}},
		{"a withdrawn stance", repo, stanceGesture("", "kyle", l2.GestureRatify, withdrawn.ID), l2.ErrNotLive, []error{l2.ErrNotFound, l2.ErrNotAllowed}},
		{"no such stance", repo, stanceGesture("", "kyle", l2.GestureRatify, "stance:nobody"), l2.ErrNotFound, []error{l2.ErrNotLive, l2.ErrNotAllowed}},
		{"a human the scope does not allow, with a carrier", only, withCarrier(stanceGesture("", "sam", l2.GestureRatify, f.onLock.ID)), l2.ErrNotAllowed, []error{l2.ErrNotLive, l2.ErrNotFound}},
		{"a human the scope does not allow, carried by an agent the scope names", carrierNamed, withCarrier(stanceGesture("", "sam", l2.GestureRatify, f.onLock.ID)), l2.ErrNotAllowed, []error{l2.ErrNotLive, l2.ErrNotFound}},
		{"a human the scope does not allow, with none", only, stanceGesture("", "sam", l2.GestureRatify, f.onLock.ID), l2.ErrNotAllowed, []error{l2.ErrNotLive, l2.ErrNotFound}},
		{"a human the scope does not allow, on a stance not live", only, stanceGesture("", "sam", l2.GestureRatify, retired.ID), l2.ErrNotAllowed, []error{l2.ErrNotLive, l2.ErrNotFound}},
		{"an agent as the principal", repo, stanceGesture("", "bot", l2.GestureRatify, f.onLock.ID), l2.ErrNotAllowed, []error{l2.ErrNotLive}},
		{"a document and a stance", repo, l2.GestureRequest{Principal: "kyle", Action: l2.GestureRatify, Stance: f.onLock.ID, Documents: []string{f.thread}}, l2.ErrInvalid, nil},
		{"a pin of a stance", repo, stanceGesture("", "kyle", l2.GesturePin, f.onLock.ID), l2.ErrInvalid, nil},
	} {
		tt.req.Event = gestureEvent()
		before := len(ledger(t, store, f.scope))
		_, recorded, err := l2.RecordGesture(ctx, pool, tt.repo, tt.req)
		if !errors.Is(err, tt.want) || recorded {
			t.Errorf("%s: RecordGesture = %v, recorded %v, want %v", tt.name, err, recorded, tt.want)
		}
		for _, other := range tt.other {
			if errors.Is(err, other) {
				t.Errorf("%s: %v is also %v, which a caller could not tell apart", tt.name, err, other)
			}
		}
		if after := len(ledger(t, store, f.scope)); after != before {
			t.Errorf("%s: the ledger grew from %d to %d", tt.name, before, after)
		}
	}
}

func withCarrier(r l2.GestureRequest) l2.GestureRequest {
	r.Carrier = "bot"
	return r
}

func TestAStanceGestureIsIdempotentOnItsEvent(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	f := newStanceFixture(t, store)

	req := withCarrier(stanceGesture(gestureEvent(), "kyle", l2.GestureRatify, f.onLock.ID))
	g := gesture(t, pool, repo, req)
	addStance(t, store, f.lock, f.thread, "the lock moves to the broker", 3)
	again, recorded, err := l2.RecordGesture(ctx, pool, repo, req)
	if err != nil || recorded || again.ID != g.ID {
		t.Fatalf("the replay after supersession = %+v, %v, recorded %v; want gesture %d back", again, err, recorded, g.ID)
	}
	if _, _, err := l2.RecordGesture(ctx, pool, repo, stanceGesture(gestureEvent(), "kyle", l2.GestureRatify, f.onLock.ID)); !errors.Is(err, l2.ErrNotLive) {
		t.Errorf("a new ratify of the superseded stance = %v, want ErrNotLive", err)
	}

	only := loadRepo(t, "scope: \"*\"\nratified_by:\n  principals: [sam]\n")
	if again, recorded, err := l2.RecordGesture(ctx, pool, only, req); err != nil || recorded || again.ID != g.ID {
		t.Errorf("the replay under a changed policy = %+v, %v, recorded %v", again, err, recorded)
	}

	for _, tt := range []struct {
		name string
		edit func(*l2.GestureRequest)
	}{
		{"another action", func(r *l2.GestureRequest) { r.Action = l2.GestureDemote }},
		{"another stance", func(r *l2.GestureRequest) { r.Stance = f.onQ.ID }},
		{"another carrier", func(r *l2.GestureRequest) { r.Carrier = "other" }},
		{"no carrier", func(r *l2.GestureRequest) { r.Carrier = "" }},
		{"another principal", func(r *l2.GestureRequest) { r.Principal = "sam" }},
		{"documents", func(r *l2.GestureRequest) { r.Stance, r.Documents = "", []string{f.thread} }},
		{"an undo", func(r *l2.GestureRequest) { r.Stance, r.Action, r.Undoes = "", l2.GestureUndo, gestureEvent() }},
	} {
		changed := req
		tt.edit(&changed)
		before := len(ledger(t, store, f.scope))
		_, recorded, err := l2.RecordGesture(ctx, pool, repo, changed)
		if !errors.Is(err, l2.ErrEventConflict) || !errors.Is(err, l2.ErrInvalid) || recorded {
			t.Errorf("%s: RecordGesture = %v, recorded %v, want ErrEventConflict", tt.name, err, recorded)
		}
		if after := len(ledger(t, store, f.scope)); after != before {
			t.Errorf("%s: the ledger grew", tt.name)
		}
	}

	undoReq := l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: req.Event, DecisionsOnly: true, Carrier: "bot"}
	undo := gesture(t, pool, repo, undoReq)
	again, recorded, err = l2.RecordGesture(ctx, pool, repo, undoReq)
	if err != nil || recorded || again.ID != undo.ID {
		t.Fatalf("the undo's replay = %+v, %v, recorded %v; want gesture %d back", again, err, recorded, undo.ID)
	}
	if again.Undoes != g.ID || again.UndoesEvent != req.Event || !slices.Equal(again.Stances, []string{f.onLock.ID}) {
		t.Errorf("the replayed undo = %+v, want what it undid exposed", again)
	}
	if _, _, err := l2.RecordGesture(ctx, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: req.Event, DecisionsOnly: true}); !errors.Is(err, l2.ErrNotUndoable) {
		t.Errorf("a new undo of the undone gesture = %v, want ErrNotUndoable", err)
	}
	if n := len(ledger(t, store, f.scope)); n != 2 {
		t.Errorf("the ledger holds %d gestures, want the ratify and its undo", n)
	}
}

func TestIdenticalStanceGesturesRecordOnce(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	f := newStanceFixture(t, store)
	base := gesture(t, pool, repo, stanceGesture(gestureEvent(), "kyle", l2.GestureDemote, f.onQ.ID))

	for _, req := range []l2.GestureRequest{
		withCarrier(stanceGesture(gestureEvent(), "kyle", l2.GestureRatify, f.onLock.ID)),
		{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: base.Event, DecisionsOnly: true},
	} {
		const n = 6
		var wg sync.WaitGroup
		var mu sync.Mutex
		var ids []int64
		fresh := 0
		for range n {
			wg.Go(func() {
				g, recorded, err := l2.RecordGesture(t.Context(), pool, repo, req)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					t.Errorf("RecordGesture(%s) = %v", req.Action, err)
					return
				}
				ids = append(ids, g.ID)
				if recorded {
					fresh++
				}
			})
		}
		wg.Wait()
		if fresh != 1 || len(ids) != n || slices.Compact(slices.Clone(ids))[0] != ids[0] || slices.Max(ids) != slices.Min(ids) {
			t.Errorf("%s: %d reported it new, ids %v; want one new and one gesture", req.Action, fresh, ids)
		}
	}
}

func TestUndoRefusesWhatCannotBeUndone(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	f := newStanceFixture(t, store)
	src := "s" + unique()
	doc := issueDoc(src, "n")
	doc.Scope, doc.L0Refs = []string{"code:acme/" + src}, []string{connector.EventID(src, "n")}
	doc.Text, doc.RawText = "t", "r"
	storeDoc(t, pool, doc)

	ratify := gesture(t, pool, repo, stanceGesture(gestureEvent(), "kyle", l2.GestureRatify, f.onLock.ID))
	undo := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: ratify.Event, DecisionsOnly: true})
	pin := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GesturePin, Documents: []string{doc.ID}})
	total := len(ledger(t, store, ""))
	for _, tt := range []struct {
		name string
		req  l2.GestureRequest
	}{
		{"an undone gesture", l2.GestureRequest{Undoes: ratify.Event, DecisionsOnly: true}},
		{"an undone gesture, unrestricted", l2.GestureRequest{Undoes: ratify.Event}},
		{"an undo", l2.GestureRequest{Undoes: undo.Event, DecisionsOnly: true}},
		{"a pin, limited to decisions", l2.GestureRequest{Undoes: pin.Event, DecisionsOnly: true}},
	} {
		tt.req.Event, tt.req.Principal, tt.req.Action = gestureEvent(), "kyle", l2.GestureUndo
		_, recorded, err := l2.RecordGesture(ctx, pool, repo, tt.req)
		if !errors.Is(err, l2.ErrNotUndoable) || errors.Is(err, l2.ErrNotFound) || errors.Is(err, l2.ErrNotAllowed) || recorded {
			t.Errorf("%s: RecordGesture = %v, recorded %v, want ErrNotUndoable", tt.name, err, recorded)
		}
		if !errors.Is(err, l2.ErrInvalid) && tt.name != "a pin, limited to decisions" {
			t.Errorf("%s: %v lost the ErrInvalid it always was", tt.name, err)
		}
	}
	if n := len(ledger(t, store, "")); n != total {
		t.Errorf("the ledger grew from %d to %d", total, n)
	}
	if g := gesture(t, pool, repo, l2.GestureRequest{Event: gestureEvent(), Principal: "kyle", Action: l2.GestureUndo, Undoes: pin.Event}); g.Undoes != pin.ID {
		t.Errorf("the undo of a pin = %+v", g)
	}
}
