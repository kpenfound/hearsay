//go:build integration

package l0_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
)

// revise turns an event into a revision of itself: the source's own version
// token in the native id, which is what makes an edit a new event rather than
// an overwrite.
func revise(ev connector.Event, token string, editedAt time.Time) connector.Event {
	ev.NativeID = ev.Payload.Artifact + "@" + token
	ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: editedAt}
	return ev
}

// Current is what everything above L0 reads: one row per artifact, and the row
// is the revision that is current by the contract's order.
func TestCurrentReturnsTheCurrentRevisionOfEachArtifact(t *testing.T) {
	store, fake, source := newStore(t)
	first := fake.NewEvent(connector.KindIssue, "acme/api#1", "as first written")
	first.Payload.Title = "an issue"
	edited := revise(first, "t2", first.Time.Add(time.Hour))
	edited.Payload.Text = "as edited"
	// An access-list re-sync is a revision like any other, and it is the one
	// that makes taking the newest load-bearing.
	resynced := revise(first, "t2+perm:private", first.Time.Add(2*time.Hour))
	resynced.Payload.Text = "as edited"
	resynced.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: "acme/api"}}

	other := fake.NewEvent(connector.KindIssue, "acme/api#2", "another issue")
	other.Payload.Title = "another"
	other.Time = first.Time.Add(time.Minute)

	for _, ev := range []connector.Event{first, edited, resynced, other} {
		if _, err := store.Append(t.Context(), ev); err != nil {
			t.Fatalf("Append(%s) = %v", ev.NativeID, err)
		}
	}

	got, err := store.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: source}})
	if err != nil {
		t.Fatalf("Current() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Current() returned %d events, want one per artifact", len(got))
	}
	if got[0].NativeID != resynced.NativeID {
		t.Errorf("Current()[0] = %q, want the newest revision %q", got[0].NativeID, resynced.NativeID)
	}
	if len(got[0].ACL) != 1 || got[0].ACL[0].Kind != connector.ACLGroup {
		t.Errorf("Current()[0].ACL = %v, want the re-synced access list", got[0].ACL)
	}
	// Oldest artifact first, by the artifact's own time.
	if got[1].Payload.Artifact != other.Payload.Artifact {
		t.Errorf("Current()[1] = %q, want %q: artifacts come back oldest first", got[1].Payload.Artifact, other.Payload.Artifact)
	}

	// One artifact's current revision, which is what a distillation reads.
	one, err := store.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: source, Artifact: "acme/api#1"}})
	if err != nil {
		t.Fatalf("Current(one artifact) = %v", err)
	}
	if len(one) != 1 || one[0].NativeID != resynced.NativeID {
		t.Fatalf("Current(one artifact) = %v, want just the newest revision", one)
	}

	// A tombstone hides the artifact from every read, this one included.
	tomb := fake.NewEvent(connector.KindTombstone, "acme/api#1:tombstone", "")
	tomb.Payload.Target = "acme/api#1"
	tomb.Payload.Author = nil
	if _, err := store.Append(t.Context(), tomb); err != nil {
		t.Fatalf("Append(tombstone) = %v", err)
	}
	after, err := store.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: source, Artifact: "acme/api#1"}})
	if err != nil {
		t.Fatalf("Current(retracted) = %v", err)
	}
	if len(after) != 0 {
		t.Errorf("Current(retracted) = %v, want nothing: a tombstone hides the artifact", after)
	}
}

// Reading one conversation is what a document is assembled from: everything
// that hangs off an artifact, and not the artifact itself.
func TestFilterByThread(t *testing.T) {
	store, fake, source := newStore(t)
	pr := fake.NewEvent(connector.KindPullRequest, "acme/api#31", "the change")
	pr.Payload.Title = "a change"

	comment := fake.NewEvent(connector.KindMessage, "acme/api#31:comment:1", "looks good")
	comment.Payload.Parent = "acme/api#31"
	comment.Payload.Thread = "acme/api#31"
	comment.Time = pr.Time.Add(time.Hour)

	// A source with no threads gives only a parent, and the contract says the
	// parent is the conversation there.
	review := fake.NewEvent(connector.KindReview, "acme/api#31:review:1", "approved")
	review.Payload.Parent = "acme/api#31"
	review.Time = pr.Time.Add(2 * time.Hour)

	// A source with three levels — a reply to a reply — hangs off its thread
	// and not off what it answers. The thread is what a document is assembled
	// from, which is why the predicate reads it first.
	nested := fake.NewEvent(connector.KindMessage, "acme/api#31:comment:2", "and another thing")
	nested.Payload.Parent = "acme/api#31:comment:1"
	nested.Payload.Thread = "acme/api#31"
	nested.Time = pr.Time.Add(3 * time.Hour)

	elsewhere := fake.NewEvent(connector.KindMessage, "acme/api#40:comment:1", "different conversation")
	elsewhere.Payload.Parent = "acme/api#40"
	elsewhere.Payload.Thread = "acme/api#40"

	for _, ev := range []connector.Event{pr, comment, review, nested, elsewhere} {
		if _, err := store.Append(t.Context(), ev); err != nil {
			t.Fatalf("Append(%s) = %v", ev.NativeID, err)
		}
	}

	for _, read := range []struct {
		name string
		fn   func(l0.ListOptions) ([]connector.Event, error)
	}{
		{"List", func(o l0.ListOptions) ([]connector.Event, error) { return store.List(t.Context(), o) }},
		{"Current", func(o l0.ListOptions) ([]connector.Event, error) { return store.Current(t.Context(), o) }},
	} {
		t.Run(read.name, func(t *testing.T) {
			got, err := read.fn(l0.ListOptions{Filter: l0.Filter{Source: source, Thread: "acme/api#31"}})
			if err != nil {
				t.Fatalf("%s() = %v", read.name, err)
			}
			var ids []string
			for _, ev := range got {
				ids = append(ids, ev.Payload.Artifact)
			}
			want := []string{comment.Payload.Artifact, review.Payload.Artifact, nested.Payload.Artifact}
			if !slices.Equal(ids, want) {
				t.Errorf("%s() = %v, want %v", read.name, ids, want)
			}
		})
	}

	// An artifact id means nothing without the source that minted it.
	if _, err := store.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Thread: "acme/api#31"}}); err == nil {
		t.Error("Current() with a thread and no source = nil, want an error")
	}
}

// A feed consumer's position survives a restart, and two replicas of one
// consumer cannot pull it backwards.
func TestFeedCursors(t *testing.T) {
	pool := newPool(t)
	cursors := l0.NewCursors(pool)
	store, fake, _ := newStore(t)
	consumer := newSourceID(t)

	if got, err := cursors.Load(t.Context(), consumer); err != nil || !got.IsZero() {
		t.Fatalf("Load(never saved) = %v, %v, want the beginning of the feed", got, err)
	}

	first, err := store.Append(t.Context(), fake.NewEvent(connector.KindMessage, "m1", "one"))
	if err != nil {
		t.Fatalf("Append() = %v", err)
	}
	second, err := store.Append(t.Context(), fake.NewEvent(connector.KindMessage, "m2", "two"))
	if err != nil {
		t.Fatalf("Append() = %v", err)
	}

	if moved, err := cursors.Save(t.Context(), consumer, second.Cursor); err != nil || !moved {
		t.Fatalf("Save() = %v, %v, want it to have moved", moved, err)
	}
	got, err := cursors.Load(t.Context(), consumer)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got != second.Cursor {
		t.Errorf("Load() = %s, want %s", got, second.Cursor)
	}

	// The other replica is behind. Its save is refused rather than making the
	// feed be read again from where it had got to.
	if moved, err := cursors.Save(t.Context(), consumer, first.Cursor); err != nil || moved {
		t.Fatalf("Save(behind) = %v, %v, want it to have been refused", moved, err)
	}
	if got, err := cursors.Load(t.Context(), consumer); err != nil || got != second.Cursor {
		t.Errorf("Load(after a save from behind) = %s, %v, want %s", got, err, second.Cursor)
	}

	// The zero cursor is the beginning of the feed, so saving it is asking to
	// go backwards.
	if moved, err := cursors.Save(t.Context(), consumer, l0.Cursor{}); err != nil || moved {
		t.Errorf("Save(zero) = %v, %v, want it to have been refused", moved, err)
	}

	// A name the column cannot hold is refused before any SQL runs: a cursor is
	// saved inside the caller's transaction, and a statement error there would
	// abort the work the cursor is being saved for.
	for _, name := range []string{"", "with a NUL \x00 in it", string(make([]byte, l0.MaxConsumerLen+1))} {
		if _, err := cursors.Save(t.Context(), name, second.Cursor); err == nil {
			t.Errorf("Save(%q) = nil, want an error", name)
		}
		if _, err := cursors.Load(t.Context(), name); err == nil {
			t.Errorf("Load(%q) = nil, want an error", name)
		}
	}
}
