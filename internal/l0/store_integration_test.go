//go:build integration

package l0_test

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/poll"
)

func newFake(t *testing.T) (*connector.Fake, string) {
	t.Helper()
	id := dbtest.UniqueID()
	return connector.NewFake(connector.SourceConfig{ID: id, Type: connector.FakeType}), id
}

func newStore(t *testing.T) (*l0.Store, *connector.Fake, string) {
	t.Helper()
	fake, source := newFake(t)
	return l0.New(dbtest.Pool(t)), fake, source
}

func TestWritingTheSameEventTwiceYieldsOneRow(t *testing.T) {
	store, fake, _ := newStore(t)
	event := fake.NewEvent(connector.KindMessage, "m1", "hello")

	first, err := store.Append(t.Context(), event)
	if err != nil {
		t.Fatalf("Append(first) = %v, want no error", err)
	}
	if !first.Stored {
		t.Error("Append(first).Stored = false, want true")
	}

	second, err := store.Append(t.Context(), event)
	if err != nil {
		t.Fatalf("Append(again) = %v, want no error", err)
	}
	if second.Stored {
		t.Error("Append(again).Stored = true, want false: a replay writes nothing")
	}
	if second.ID != first.ID || second.Cursor != first.Cursor {
		t.Errorf("Append(again) = %+v, want the row the first call wrote, %+v", second, first)
	}

	if want := connector.EventID(event.Source, event.NativeID); first.ID != want {
		t.Errorf("Append().ID = %q, want the derived id %q", first.ID, want)
	}
	assertCounts(t, store, event.Source, connector.KindMessage, 1, 1)
}

func TestAppendRefusesToRewriteAnEvent(t *testing.T) {
	store, fake, _ := newStore(t)
	event := fake.NewEvent(connector.KindMessage, "m1", "hello")
	if _, err := store.Append(t.Context(), event); err != nil {
		t.Fatalf("Append() = %v, want no error", err)
	}

	edited := event
	edited.Payload.Text = "hello again"
	if _, err := store.Append(t.Context(), edited); !errors.Is(err, l0.ErrRewrite) {
		t.Fatalf("Append(rewritten) = %v, want l0.ErrRewrite", err)
	}

	stored, err := store.Get(t.Context(), connector.EventID(event.Source, event.NativeID))
	if err != nil {
		t.Fatalf("Get() = %v, want no error", err)
	}
	if stored.Payload.Text != "hello" {
		t.Errorf("payload.text = %q, want the text that was stored first, %q", stored.Payload.Text, "hello")
	}
	if !stored.Time.Equal(event.Time) {
		t.Errorf("time = %s, want %s", stored.Time, event.Time)
	}
	if stored.Time.Location() != time.UTC {
		t.Errorf("time came back in %s, want UTC", stored.Time.Location())
	}
	assertCounts(t, store, event.Source, connector.KindMessage, 1, 1)
}

func TestAReplayWithADifferentSpellingOfOnePayloadIsStillAReplay(t *testing.T) {
	store, fake, _ := newStore(t)
	event := fake.NewEvent(connector.KindMessage, "m1", "hello")
	event.Payload.Native = []byte(`{"a":1,"b":2}`)
	if _, err := store.Append(t.Context(), event); err != nil {
		t.Fatalf("Append() = %v, want no error", err)
	}

	respelled := event
	respelled.Payload.Native = []byte("{\n  \"b\": 2,\n  \"a\": 1\n}")
	again, err := store.Append(t.Context(), respelled)
	if err != nil {
		t.Fatalf("Append(respelled) = %v, want no error", err)
	}
	if again.Stored {
		t.Error("Append(respelled).Stored = true, want false")
	}
}

func TestAnEditIsANewEventAndTheHistoryIsOrdered(t *testing.T) {
	store, fake, _ := newStore(t)
	first := fake.NewEvent(connector.KindMessage, "m1", "hello")
	second := fake.NewEvent(connector.KindMessage, "m1", "hello, edited")
	second.NativeID = "m1@r2"
	second.Payload.Revision = &connector.Revision{Token: "r2", EditedAt: first.Time.Add(time.Hour)}
	third := fake.NewEvent(connector.KindMessage, "m1", "hello, edited twice")
	third.NativeID = "m1@r3"
	third.Payload.Revision = &connector.Revision{Token: "r3", EditedAt: first.Time.Add(2 * time.Hour)}

	for _, event := range []connector.Event{third, first, second} {
		if _, err := store.Append(t.Context(), event); err != nil {
			t.Fatalf("Append(%s) = %v, want no error", event.NativeID, err)
		}
	}

	for _, tt := range []struct {
		name string
		opts l0.ListOptions
		want []string
	}{
		{name: "the whole history", opts: l0.ListOptions{}, want: []string{"m1", "m1@r2", "m1@r3"}},
		{name: "newest first", opts: l0.ListOptions{Newest: true}, want: []string{"m1@r3", "m1@r2", "m1"}},
		{name: "the oldest one", opts: l0.ListOptions{Limit: 1}, want: []string{"m1"}},
		{name: "the newest one", opts: l0.ListOptions{Limit: 1, Newest: true}, want: []string{"m1@r3"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			opts.Source, opts.Artifact = first.Source, "m1"
			history, err := store.List(t.Context(), opts)
			if err != nil {
				t.Fatalf("List(%+v) = %v, want no error", opts, err)
			}
			if got := nativeIDs(history); !slices.Equal(got, tt.want) {
				t.Errorf("List(%+v) = %v, want %v", opts, got, tt.want)
			}
		})
	}
	assertCounts(t, store, first.Source, connector.KindMessage, 3, 3)
}

func TestATombstoneHidesTheEventAndKeepsTheRow(t *testing.T) {
	store, fake, _ := newStore(t)
	event := fake.NewEvent(connector.KindMessage, "m1", "a pasted secret")
	appended, err := store.Append(t.Context(), event)
	if err != nil {
		t.Fatalf("Append() = %v, want no error", err)
	}
	if got, want := feed(t, store, l0.Cursor{}, event.Source, []string{"m1"}), []string{"m1"}; !slices.Equal(got, want) {
		t.Fatalf("the feed before the tombstone = %v, want %v", got, want)
	}

	tombstone := fake.NewEvent(connector.KindTombstone, "m1:tombstone", "")
	tombstone.Payload.Target = "m1"
	if _, err := store.Append(t.Context(), tombstone); err != nil {
		t.Fatalf("Append(tombstone) = %v, want no error", err)
	}

	if _, err := store.Get(t.Context(), appended.ID); !errors.Is(err, l0.ErrRetracted) {
		t.Errorf("Get(tombstoned) = %v, want l0.ErrRetracted", err)
	}
	if _, err := store.Get(t.Context(), "evt:"+event.Source+":nothing"); !errors.Is(err, l0.ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want l0.ErrNotFound: a retracted event is not an unknown one", err)
	}

	listed, err := store.List(t.Context(), l0.ListOptions{Source: event.Source})
	if err != nil {
		t.Fatalf("List() = %v, want no error", err)
	}
	if got := nativeIDs(listed); !slices.Equal(got, []string{"m1:tombstone"}) {
		t.Errorf("List() = %v, want only the tombstone", got)
	}

	want := []string{"m1:tombstone"}
	if got := feed(t, store, l0.Cursor{}, event.Source, want); !slices.Equal(got, want) {
		t.Errorf("the feed after the tombstone = %v, want only the tombstone", got)
	}

	assertCounts(t, store, event.Source, connector.KindMessage, 1, 0)
	assertCounts(t, store, event.Source, connector.KindTombstone, 1, 1)
}

func TestRetractionAndReentryKeepHistoryAndCurrentState(t *testing.T) {
	store, fake, source := newStore(t)
	first := fake.NewEvent(connector.KindDocument, "file@r1", "old")
	first.Payload.Artifact = "file"
	first.Payload.Revision = &connector.Revision{Token: "r1", EditedAt: first.Time}
	tomb := fake.NewEvent(connector.KindTombstone, "file:tombstone:1", "")
	tomb.Payload.Target = "file"
	for _, ev := range []connector.Event{first, tomb} {
		if _, err := store.Append(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := store.CurrentArtifact(t.Context(), source, "file"); err != nil || ok {
		t.Fatalf("retracted current = %v, %v", ok, err)
	}
	last, ok, err := store.LastRetraction(t.Context(), source, "file")
	if err != nil || !ok || last.NativeID != tomb.NativeID {
		t.Fatalf("last retraction = %+v, %v, %v", last, ok, err)
	}
	reentry := first
	reentry.NativeID = "file@r1+return:1"
	reentry.Payload.Revision = &connector.Revision{Token: "r1+return:1", EditedAt: first.Time}
	reentry.Payload.Text = "new"
	reentry.ACL = connector.ACL{{Kind: connector.ACLPublic}}
	if _, err := store.Append(t.Context(), reentry); err != nil {
		t.Fatal(err)
	}
	if replay, err := store.Append(t.Context(), tomb); err != nil || replay.Stored {
		t.Fatalf("replayed tombstone = %+v, %v", replay, err)
	}
	if replay, err := store.Append(t.Context(), reentry); err != nil || replay.Stored {
		t.Fatalf("replayed reentry = %+v, %v", replay, err)
	}
	current, ok, err := store.CurrentArtifact(t.Context(), source, "file")
	if err != nil || !ok || current.Payload.Text != "new" || !current.ACL.Equal(reentry.ACL) {
		t.Fatalf("restored current = %+v, %v, %v", current, ok, err)
	}
	if _, err := store.Get(t.Context(), connector.EventID(source, first.NativeID)); !errors.Is(err, l0.ErrRetracted) {
		t.Errorf("old revision = %v", err)
	}
	if _, err := store.Get(t.Context(), connector.EventID(source, reentry.NativeID)); err != nil {
		t.Errorf("reentry = %v", err)
	}
	visible, err := store.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: source, Artifact: "file"}})
	if err != nil || !slices.Equal(nativeIDs(visible), []string{reentry.NativeID}) {
		t.Errorf("visible history = %v, %v", nativeIDs(visible), err)
	}
	if got := feed(t, store, l0.Cursor{}, source, []string{tomb.NativeID, reentry.NativeID}); !slices.Equal(got, []string{tomb.NativeID, reentry.NativeID}) {
		t.Errorf("feed = %v", got)
	}
	rewritten := reentry
	rewritten.Payload.Text = "different"
	if _, err := store.Append(t.Context(), rewritten); !errors.Is(err, l0.ErrRewrite) {
		t.Errorf("rewrite = %v", err)
	}
	secondTomb := tomb
	secondTomb.NativeID = "file:tombstone:2"
	secondTomb.Payload.Artifact = secondTomb.NativeID
	if _, err := store.Append(t.Context(), secondTomb); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.CurrentArtifact(t.Context(), source, "file"); err != nil || ok {
		t.Errorf("second retraction current = %v, %v", ok, err)
	}
}

func TestRetractedReadsOnlyWhatATombstoneHides(t *testing.T) {
	store, fake, source := newStore(t)
	other, otherSource := newFake(t)

	first := fake.NewEvent(connector.KindMessage, "m1", "a pasted secret")
	first.Payload.Parent, first.Payload.Thread = "issue", "issue"
	edited := first
	edited.NativeID = "m1@r2"
	edited.Payload.Text = "a pasted secret, edited"
	edited.Payload.Revision = &connector.Revision{Token: "r2", EditedAt: first.Time.Add(time.Hour)}
	kept := fake.NewEvent(connector.KindMessage, "m2", "still here")
	elsewhere := other.NewEvent(connector.KindMessage, "m2", "somewhere else")
	elsewhereTombstone := other.NewEvent(connector.KindTombstone, "m2:tombstone", "")
	elsewhereTombstone.Payload.Target = "m2"
	for _, ev := range []connector.Event{edited, first, kept, elsewhere, elsewhereTombstone} {
		if _, err := store.Append(t.Context(), ev); err != nil {
			t.Fatalf("Append(%s) = %v, want no error", ev.NativeID, err)
		}
	}

	if _, err := store.Retracted(t.Context(), source, "m1"); !errors.Is(err, l0.ErrNotFound) {
		t.Errorf("Retracted(before the tombstone) = %v, want l0.ErrNotFound", err)
	}

	tombstone := fake.NewEvent(connector.KindTombstone, "m1:tombstone", "")
	tombstone.Payload.Target = "m1"
	if _, err := store.Append(t.Context(), tombstone); err != nil {
		t.Fatalf("Append(tombstone) = %v, want no error", err)
	}

	got, err := store.Retracted(t.Context(), source, "m1")
	if err != nil {
		t.Fatalf("Retracted(m1) = %v, want no error", err)
	}
	if got.NativeID != "m1@r2" || got.Payload.Thread != "issue" {
		t.Errorf("Retracted(m1) = %s in thread %q, want the current revision m1@r2 in thread %q", got.NativeID, got.Payload.Thread, "issue")
	}

	for _, tt := range []struct{ name, source, artifact string }{
		{name: "an artifact nothing retracted", source: source, artifact: "m2"},
		{name: "an artifact the source never held", source: source, artifact: "m9"},
		{name: "an artifact retracted in this source, asked of another", source: otherSource, artifact: "m1"},
	} {
		if _, err := store.Retracted(t.Context(), tt.source, tt.artifact); !errors.Is(err, l0.ErrNotFound) {
			t.Errorf("Retracted(%s) = %v, want l0.ErrNotFound", tt.name, err)
		}
	}
	if _, err := store.Retracted(t.Context(), otherSource, "m2"); err != nil {
		t.Errorf("Retracted(the other source's own retraction) = %v, want it found", err)
	}
	if _, err := store.Retracted(t.Context(), "", "m1"); err == nil || errors.Is(err, l0.ErrNotFound) {
		t.Errorf("Retracted(no source) = %v, want a refusal", err)
	}
}

func TestTheChangeFeedResumesFromACursor(t *testing.T) {
	store, fake, source := newStore(t)
	var marker l0.Cursor
	for i, text := range []string{"one", "two", "three"} {
		event := fake.NewEvent(connector.KindMessage, "m"+strconv.Itoa(i), text)
		appended, err := store.Append(t.Context(), event)
		if err != nil {
			t.Fatalf("Append(%s) = %v, want no error", event.NativeID, err)
		}
		if i == 0 {
			marker = appended.Cursor
		}
	}

	want := []string{"m0", "m1", "m2"}
	if got := feed(t, store, l0.Cursor{}, source, want); !slices.Equal(got, want) {
		t.Errorf("reading the feed from the beginning saw %v, want %v", got, want)
	}

	want = []string{"m1", "m2"}
	if got := feed(t, store, marker, source, want); !slices.Equal(got, want) {
		t.Errorf("Changes(from the first event) = %v, want %v", got, want)
	}
}

func TestTheChangeFeedDoesNotSkipASlowTransaction(t *testing.T) {
	pool := dbtest.Pool(t)
	fake, source := newFake(t)
	store := l0.New(pool)

	marker, err := store.Append(t.Context(), fake.NewEvent(connector.KindMessage, "marker", "start"))
	if err != nil {
		t.Fatalf("Append(marker) = %v, want no error", err)
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("Begin() = %v, want no error", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := l0.New(tx).Append(t.Context(), fake.NewEvent(connector.KindMessage, "slow", "written first")); err != nil {
		t.Fatalf("Append(slow) = %v, want no error", err)
	}
	if _, err := store.Append(t.Context(), fake.NewEvent(connector.KindMessage, "fast", "committed first")); err != nil {
		t.Fatalf("Append(fast) = %v, want no error", err)
	}

	held, err := store.Changes(t.Context(), marker.Cursor, l0.Filter{Source: source}, l0.MaxLimit)
	if err != nil {
		t.Fatalf("Changes(while a write is in flight) = %v, want no error", err)
	}
	if got := nativeIDs(eventsOf(held)); len(got) != 0 {
		t.Errorf("the feed handed out %v while a write was in flight, want nothing", got)
	}

	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("Commit() = %v, want no error", err)
	}
	want := []string{"slow", "fast"}
	if got := feed(t, store, marker.Cursor, source, want); !slices.Equal(got, want) {
		t.Errorf("the feed after the commit = %v, want %v", got, want)
	}
}

func TestTheChangeFeedTakesAFilter(t *testing.T) {
	store, fake, source := newStore(t)
	other, otherSource := newFake(t)

	marker, err := store.Append(t.Context(), fake.NewEvent(connector.KindMessage, "marker", "start"))
	if err != nil {
		t.Fatalf("Append(marker) = %v, want no error", err)
	}
	for _, event := range []connector.Event{
		fake.NewEvent(connector.KindMessage, "m1", "mine"),
		other.NewEvent(connector.KindMessage, "m1", "somebody else's"),
		fake.NewEvent(connector.KindIssue, "i1", "an issue of mine"),
	} {
		if _, err := store.Append(t.Context(), event); err != nil {
			t.Fatalf("Append(%s) = %v, want no error", event.NativeID, err)
		}
	}

	tests := []struct {
		name   string
		filter l0.Filter
		want   []string
	}{
		{name: "one source", filter: l0.Filter{Source: source}, want: []string{"m1", "i1"}},
		{name: "another source", filter: l0.Filter{Source: otherSource}, want: []string{"m1"}},
		{name: "one kind of one source", filter: l0.Filter{Source: source, Kind: connector.KindIssue}, want: []string{"i1"}},
		{name: "one artifact", filter: l0.Filter{Source: source, Artifact: "m1"}, want: []string{"m1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := feedWhere(t, store, marker.Cursor, tt.filter, tt.want); !slices.Equal(got, tt.want) {
				t.Errorf("Changes(%+v) = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}

	if _, err := store.Changes(t.Context(), marker.Cursor, l0.Filter{Artifact: "m1"}, l0.MaxLimit); err == nil {
		t.Error("Changes(artifact without source) = nil, want an error")
	}
}

func TestTheChangeFeedOrdersOnTheTransactionNotTheSequence(t *testing.T) {
	pool := dbtest.Pool(t)
	fake, source := newFake(t)
	store := l0.New(pool)
	marker, err := store.Append(t.Context(), fake.NewEvent(connector.KindMessage, "marker", "start"))
	if err != nil {
		t.Fatalf("Append(marker) = %v, want no error", err)
	}

	early, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("Begin(early) = %v, want no error", err)
	}
	defer func() { _ = early.Rollback(t.Context()) }()
	if _, err := early.Exec(t.Context(), `SELECT pg_current_xact_id()`); err != nil {
		t.Fatalf("assigning a transaction id: %v", err)
	}

	late, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("Begin(late) = %v, want no error", err)
	}
	defer func() { _ = late.Rollback(t.Context()) }()
	if _, err := l0.New(late).Append(t.Context(), fake.NewEvent(connector.KindMessage, "late", "inserted first")); err != nil {
		t.Fatalf("Append(late) = %v, want no error", err)
	}
	if _, err := l0.New(early).Append(t.Context(), fake.NewEvent(connector.KindMessage, "early", "inserted second")); err != nil {
		t.Fatalf("Append(early) = %v, want no error", err)
	}
	if err := late.Commit(t.Context()); err != nil {
		t.Fatalf("Commit(late) = %v, want no error", err)
	}
	if err := early.Commit(t.Context()); err != nil {
		t.Fatalf("Commit(early) = %v, want no error", err)
	}

	want := []string{"early", "late"}
	if got := feed(t, store, marker.Cursor, source, want); !slices.Equal(got, want) {
		t.Errorf("the feed = %v, want %v: the order is the transactions', not the sequence's", got, want)
	}
}

func TestTheTableRefusesARowThatBreaksTheContract(t *testing.T) {
	pool := dbtest.Pool(t)
	_, source := newFake(t)
	token, edited := "r2", time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		constraint string
		nativeID   string
		artifact   string
		token      *string
		edited     *time.Time
		target     *string
		acl        string
	}{
		{
			name:     "a row the contract allows",
			nativeID: "m1", artifact: "m1", acl: `[{"kind":"public"}]`,
		},
		{
			name:       "a native id that is neither the artifact nor a revision of it",
			constraint: "l0_events_native_id_is_the_artifact_or_a_revision",
			nativeID:   "m2", artifact: "m1", acl: `[{"kind":"public"}]`,
		},
		{
			name:       "a revision token the native id does not carry",
			constraint: "l0_events_native_id_is_the_artifact_or_a_revision",
			nativeID:   "m1", artifact: "m1", token: &token, acl: `[{"kind":"public"}]`,
		},
		{
			name:       "an edit that precedes the artifact it edits",
			constraint: "l0_events_revision_edited_at_is_the_revisions_own",
			nativeID:   "m1@r2", artifact: "m1", token: &token, edited: &edited, acl: `[{"kind":"public"}]`,
		},
		{
			name:       "an edit time on an observation with no revision",
			constraint: "l0_events_revision_edited_at_is_the_revisions_own",
			nativeID:   "m1", artifact: "m1", edited: &edited, acl: `[{"kind":"public"}]`,
		},
		{
			name:       "a tombstone that retracts itself",
			constraint: "l0_events_tombstone_is_its_own_artifact",
			nativeID:   "m1", artifact: "m1", target: ptr("m1"), acl: `[{"kind":"public"}]`,
		},
		{
			name:       "an event nobody may read",
			constraint: "l0_events_acl_is_not_empty",
			nativeID:   "m1", artifact: "m1", acl: `[]`,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(t.Context(), `
INSERT INTO l0_events (id, source, native_id, kind, artifact, revision_token, revision_edited_at, target, occurred_at, payload, acl)
VALUES ($1, $2, $3, 'message', $4, $5, $6, $7, '2024-01-01T00:00:00Z', '{}', $8)`,
				"evt:"+source+":row"+strconv.Itoa(i), source, tt.nativeID, tt.artifact,
				tt.token, tt.edited, tt.target, tt.acl)

			if tt.constraint == "" {
				if err != nil {
					t.Fatalf("inserting a row the contract allows = %v, want no error", err)
				}
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("the insert = %v, want a constraint violation", err)
			}
			if pgErr.ConstraintName != tt.constraint {
				t.Errorf("the insert violated %q (%s), want %q", pgErr.ConstraintName, pgErr.Code, tt.constraint)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func assertCounts(t *testing.T, store *l0.Store, source string, kind connector.Kind, events, visible int64) {
	t.Helper()
	counts, err := store.Counts(t.Context())
	if err != nil {
		t.Fatalf("Counts() = %v, want no error", err)
	}
	for _, c := range counts {
		if c.Source != source || c.Kind != kind {
			continue
		}
		if c.Events != events || c.Visible != visible {
			t.Errorf("Counts()[%s %s] = %d events, %d visible; want %d and %d", source, kind, c.Events, c.Visible, events, visible)
		}
		return
	}
	if events != 0 {
		t.Errorf("Counts() has no entry for %s %s, want %d events", source, kind, events)
	}
}

func feed(t *testing.T, store *l0.Store, from l0.Cursor, source string, want []string) []string {
	t.Helper()
	return feedWhere(t, store, from, l0.Filter{Source: source}, want)
}

func feedWhere(t *testing.T, store *l0.Store, from l0.Cursor, filter l0.Filter, want []string) []string {
	t.Helper()
	var got []string
	poll.Check(10*time.Second, func() bool {
		changes, err := store.Changes(t.Context(), from, filter, l0.MaxLimit)
		if err != nil {
			t.Fatalf("Changes(%+v) = %v, want no error", filter, err)
		}
		got = nativeIDs(eventsOf(changes))
		return slices.Equal(got, want)
	})
	return got
}

func nativeIDs(events []connector.Event) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.NativeID)
	}
	return ids
}

func eventsOf(changes []l0.Change) []connector.Event {
	events := make([]connector.Event, 0, len(changes))
	for _, change := range changes {
		events = append(events, change.Event)
	}
	return events
}
