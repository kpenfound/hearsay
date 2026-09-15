//go:build integration

package l0_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
)

var (
	_ connector.ResyncStore    = (*l0.Resyncs)(nil)
	_ connector.ExposureReader = (*l0.Store)(nil)
)

// A re-sync is recorded, walked and settled across what could be separate
// processes, and a request that arrives during a walk is not settled by it.
func TestResyncsRoundTrip(t *testing.T) {
	pool := newPool(t)
	store := l0.NewResyncs(pool)
	source := newSourceID(t)
	get := func(container string) connector.Resync {
		t.Helper()
		records, err := store.Resyncs(t.Context(), source)
		if err != nil {
			t.Fatalf("Resyncs() = %v", err)
		}
		for _, r := range records {
			if r.Container == container {
				return r
			}
		}
		return connector.Resync{}
	}

	if records, err := store.Resyncs(t.Context(), source); err != nil || len(records) != 0 {
		t.Fatalf("Resyncs(new source) = %+v, %v, want none", records, err)
	}
	// Saving the position of a re-sync nobody owes writes nothing.
	if err := store.Save(t.Context(), source, connector.Resync{Container: "acme/api", Generation: 1, Cursor: "nowhere"}); err != nil {
		t.Fatalf("Save(not owed) = %v", err)
	}
	if r := get("acme/api"); r.Generation != 0 {
		t.Fatalf("Save(not owed) created %+v", r)
	}

	if err := store.Owe(t.Context(), source, "acme/api"); err != nil {
		t.Fatalf("Owe() = %v", err)
	}
	walk := get("acme/api")
	walk.Cursor = "page-2"
	if err := store.Save(t.Context(), source, walk); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	walk = get("acme/api")
	if !walk.Owed || walk.Cursor != "page-2" || walk.Generation != 1 || !walk.ResyncedAt.IsZero() {
		t.Fatalf("after Owe and Save: %+v", walk)
	}

	// Asked again mid-walk: the walk starts again, the old walk's next page
	// does not move it, and finishing the old walk does not settle it.
	if err := store.Owe(t.Context(), source, "acme/api"); err != nil {
		t.Fatalf("Owe(again) = %v", err)
	}
	if r := get("acme/api"); r.Cursor != "" || r.Generation != 2 {
		t.Errorf("after a second Owe: %+v, want the walk from the beginning at generation 2", r)
	}
	stale := walk
	stale.Cursor = "page-3"
	if err := store.Save(t.Context(), source, stale); err != nil {
		t.Fatalf("Save(an older generation) = %v", err)
	}
	if r := get("acme/api"); r.Cursor != "" {
		t.Errorf("Save(an older generation) moved the cursor to %q", r.Cursor)
	}
	finished, err := store.Finish(t.Context(), source, walk)
	if err != nil || finished {
		t.Fatalf("Finish(an older generation) = %v, %v, want false", finished, err)
	}
	again := get("acme/api")
	if !again.Owed || again.Cursor != "" || !again.ResyncedAt.IsZero() {
		t.Fatalf("after finishing an older generation: %+v, want owed from the beginning", again)
	}

	finished, err = store.Finish(t.Context(), source, again)
	if err != nil || !finished {
		t.Fatalf("Finish() = %v, %v, want true", finished, err)
	}
	done := get("acme/api")
	if done.Owed || done.ResyncedAt.IsZero() {
		t.Fatalf("after Finish: %+v, want settled with a finish time", done)
	}

	// Owed again after it was settled: from the beginning, keeping when the last
	// one finished.
	if err := store.Owe(t.Context(), source, "acme/api"); err != nil {
		t.Fatalf("Owe(after Finish) = %v", err)
	}
	if r := get("acme/api"); !r.Owed || r.Cursor != "" || !r.ResyncedAt.Equal(done.ResyncedAt) {
		t.Errorf("owed again: %+v, want owed from the beginning with resynced_at %v", r, done.ResyncedAt)
	}

	// One row per container, in container order.
	if err := store.Owe(t.Context(), source, "acme/aaa"); err != nil {
		t.Fatalf("Owe(another container) = %v", err)
	}
	records, err := store.Resyncs(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	var containers []string
	for _, r := range records {
		containers = append(containers, r.Container)
	}
	if want := []string{"acme/aaa", "acme/api"}; !slices.Equal(containers, want) {
		t.Errorf("containers = %v, want %v", containers, want)
	}

	if err := store.Owe(t.Context(), source, ""); err == nil {
		t.Error("Owe(no container) = nil, want an error")
	}
	if err := store.Owe(t.Context(), "Not A Source", "acme/api"); err == nil {
		t.Error("Owe(not a source id) = nil, want an error")
	}
}

// Exposed reads each artifact's current revision, in the contract's revision
// order and past retractions: an artifact re-emitted private is not exposed, one
// whose newest edit is public is, and a retracted one and a tombstone are not.
func TestExposedReadsTheCurrentRevision(t *testing.T) {
	store, fake, source := newStore(t)
	private := func(ev connector.Event, token string) connector.Event {
		ev.NativeID = ev.Payload.Artifact + "@" + token
		revision := connector.Revision{Token: token}
		if ev.Payload.Revision != nil {
			revision.EditedAt = ev.Payload.Revision.EditedAt
		}
		ev.Payload.Revision = &revision
		ev.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: "C123"}}
		return ev
	}
	edited := func(ev connector.Event, token string, at time.Time) connector.Event {
		ev.NativeID = ev.Payload.Artifact + "@" + token
		ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: at}
		return ev
	}
	at := time.Date(2026, time.September, 10, 0, 0, 0, 0, time.UTC)

	resynced := fake.NewEvent(connector.KindMessage, "resynced", "x")
	gone := fake.NewEvent(connector.KindMessage, "gone", "x")
	tombstone := fake.NewEvent(connector.KindTombstone, "gone:tombstone", "")
	tombstone.Payload.Target = "gone"
	// The newer edit arrives first, so arrival order and revision order differ.
	newer := edited(fake.NewEvent(connector.KindMessage, "edited", "x"), "2", at.Add(time.Hour))
	older := private(edited(fake.NewEvent(connector.KindMessage, "edited", "x"), "1", at), "1+perm:private")

	check := func(want []string) {
		t.Helper()
		exposed, err := store.Exposed(t.Context(), source)
		if err != nil {
			t.Fatalf("Exposed() = %v", err)
		}
		var got []string
		for _, e := range exposed {
			got = append(got, e.Container)
			if e.LastPublic.IsZero() {
				t.Errorf("%s: no LastPublic", e.Container)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("Exposed() = %v, want %v", got, want)
		}
	}

	for _, ev := range []connector.Event{resynced, private(resynced, "perm:private"), gone, tombstone} {
		if _, err := store.Append(t.Context(), ev); err != nil {
			t.Fatalf("Append(%s) = %v", ev.NativeID, err)
		}
	}
	check(nil)

	for _, ev := range []connector.Event{newer, older} {
		if _, err := store.Append(t.Context(), ev); err != nil {
			t.Fatalf("Append(%s) = %v", ev.NativeID, err)
		}
	}
	check([]string{newer.Payload.Container.NativeID})
}
