//go:build integration

package l0_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

// newPool connects to the database the integration-test check brings up. It
// uses db.Connect rather than Open, so a database the migrations have not been
// run against fails here saying so: tests never build a schema of their own.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// sources is what keeps tests out of each other's way. L0 is append-only and
// the database outlives one test, so a test gets a source id nothing else uses
// rather than truncating a table other tests are reading.
var sources atomic.Int64

// newFake returns a connector that emits from a source nothing else uses, and
// that source's id.
func newFake(t *testing.T) (*connector.Fake, string) {
	t.Helper()
	id := "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(sources.Add(1), 36)
	return connector.NewFake(connector.SourceConfig{ID: id, Type: connector.FakeType}), id
}

func newStore(t *testing.T) (*l0.Store, *connector.Fake, string) {
	t.Helper()
	fake, source := newFake(t)
	return l0.New(newPool(t)), fake, source
}

// The acceptance criterion: writing the same event twice yields one row.
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

	// The id a connector never set is the derived one, and it is what Get takes.
	if want := connector.EventID(event.Source, event.NativeID); first.ID != want {
		t.Errorf("Append().ID = %q, want the derived id %q", first.ID, want)
	}
	assertCounts(t, store, event.Source, connector.KindMessage, 1, 1)
}

// A connector that re-emits an artifact it changed without changing the
// revision token in its native id is breaking docs/connector-contract.md. L0 is
// append-only, so the stored row wins and the caller is told.
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
	// The instant survives the round trip, and comes back in UTC rather than in
	// whatever zone the server's session happens to be in.
	if !stored.Time.Equal(event.Time) {
		t.Errorf("time = %s, want %s", stored.Time, event.Time)
	}
	if stored.Time.Location() != time.UTC {
		t.Errorf("time came back in %s, want UTC", stored.Time.Location())
	}
	assertCounts(t, store, event.Source, connector.KindMessage, 1, 1)
}

// The same payload spelled differently is a replay, not a rewrite: the
// comparison is Postgres's jsonb equality, which ignores key order and
// whitespace, rather than a hash of whatever bytes the connector marshalled.
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

// An edit carries a new revision token in its native id, so it is a new event
// beside the first appearance rather than an overwrite of it.
//
// The default order is oldest first, which is docs/connector-contract.md's
// ordering of an artifact's revisions reversed; ListOptions.Newest is the
// contract's own direction, with the current revision first.
func TestAnEditIsANewEventAndTheHistoryIsOrdered(t *testing.T) {
	store, fake, _ := newStore(t)
	first := fake.NewEvent(connector.KindMessage, "m1", "hello")
	second := fake.NewEvent(connector.KindMessage, "m1", "hello, edited")
	second.NativeID = "m1@r2"
	second.Payload.Revision = &connector.Revision{Token: "r2", EditedAt: first.Time.Add(time.Hour)}
	third := fake.NewEvent(connector.KindMessage, "m1", "hello, edited twice")
	third.NativeID = "m1@r3"
	third.Payload.Revision = &connector.Revision{Token: "r3", EditedAt: first.Time.Add(2 * time.Hour)}

	// Out of order on the way in, to prove the order is the data's and not
	// arrival's.
	for _, event := range []connector.Event{third, first, second} {
		if _, err := store.Append(t.Context(), event); err != nil {
			t.Fatalf("Append(%s) = %v, want no error", event.NativeID, err)
		}
	}

	// Oldest first, newest first, and a limit that cuts from the far end of
	// whichever order is in force.
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

// The acceptance criterion: tombstoning hides an event from the feed and from a
// read by id, and keeps the row.
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

	// The feed carries the tombstone and not what it retracts, which is how a
	// consumer learns to re-derive without the event.
	want := []string{"m1:tombstone"}
	if got := feed(t, store, l0.Cursor{}, event.Source, want); !slices.Equal(got, want) {
		t.Errorf("the feed after the tombstone = %v, want only the tombstone", got)
	}

	// And the row is still there: append-only means the count does not drop.
	assertCounts(t, store, event.Source, connector.KindMessage, 1, 0)
	assertCounts(t, store, event.Source, connector.KindTombstone, 1, 1)
}

// The change feed is what the distiller consumes, so a reader must be able to
// stop, keep the cursor, and carry on where it was.
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

	// A cursor is exclusive: resuming after the first event does not repeat it.
	want = []string{"m1", "m2"}
	if got := feed(t, store, marker, source, want); !slices.Equal(got, want) {
		t.Errorf("Changes(from the first event) = %v, want %v", got, want)
	}
}

// The reason the cursor is a pair. A sequence number is handed out before the
// transaction holding it commits, so a row can become readable *after* a row
// with a higher number. A feed ordered on the number alone would hand out the
// higher one, move the cursor past it, and never return the lower one at all.
func TestTheChangeFeedDoesNotSkipASlowTransaction(t *testing.T) {
	pool := newPool(t)
	fake, source := newFake(t)
	store := l0.New(pool)

	// A marker, so the assertions are about what happens after it.
	marker, err := store.Append(t.Context(), fake.NewEvent(connector.KindMessage, "marker", "start"))
	if err != nil {
		t.Fatalf("Append(marker) = %v, want no error", err)
	}

	// `slow` takes its sequence number first and commits last.
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

	// While `slow` is in flight the feed holds at the marker rather than
	// handing out `fast` and stranding `slow` behind the cursor.
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

// The feed takes the same filter a listing does, so an operator or a consumer
// watching one source is handed that source and not the store. The cursor is a
// position in the whole feed either way — the filter narrows what comes back,
// not where a reader is.
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
			// Patient for the same reason feed() is: a transaction in flight
			// anywhere holds the feed back.
			deadline := time.Now().Add(10 * time.Second)
			var got []string
			for {
				changes, err := store.Changes(t.Context(), marker.Cursor, tt.filter, l0.MaxLimit)
				if err != nil {
					t.Fatalf("Changes(%+v) = %v, want no error", tt.filter, err)
				}
				got = nativeIDs(eventsOf(changes))
				if slices.Equal(got, tt.want) || time.Now().After(deadline) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Changes(%+v) = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}

	// An artifact id means nothing outside its source, on the feed as on a
	// listing, and the same rule says so.
	if _, err := store.Changes(t.Context(), marker.Cursor, l0.Filter{Artifact: "m1"}, l0.MaxLimit); err == nil {
		t.Error("Changes(artifact without source) = nil, want an error")
	}
}

// The other half of why the cursor is a pair: sequence order and transaction
// order can disagree. A transaction that takes its id first can insert second,
// so the row with the lower sequence number belongs to the later transaction.
// The feed orders on the transaction, and a reader whose cursor is a sequence
// number alone would be handed the later one first and then handed the earlier
// one again on the next read.
func TestTheChangeFeedOrdersOnTheTransactionNotTheSequence(t *testing.T) {
	pool := newPool(t)
	fake, source := newFake(t)
	store := l0.New(pool)
	marker, err := store.Append(t.Context(), fake.NewEvent(connector.KindMessage, "marker", "start"))
	if err != nil {
		t.Fatalf("Append(marker) = %v, want no error", err)
	}

	// `early` takes the lower transaction id and the higher sequence number.
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

// The table refuses a row that breaks the contract even if something other than
// the ingest path writes it, which is what the CHECK constraints are for. Each
// case names the constraint it expects, so a row rejected for some other reason
// — a typo in the statement, above all — is not mistaken for the rule holding.
func TestTheTableRefusesARowThatBreaksTheContract(t *testing.T) {
	pool := newPool(t)
	_, source := newFake(t)
	token, edited := "r2", time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		constraint string // empty means the row is a good one
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
			// occurred_at is 2024 and the edit time above is 2020, so the
			// case about an edit preceding its artifact really does.
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

// feed reads one source's events off the change feed until they are the ones
// wanted, or a deadline passes.
//
// It waits because the feed deliberately holds an event back until the
// transaction that wrote it has finished, and *any* transaction older than that
// one holds it too — including one belonging to something else entirely, which
// on a shared test database is every other package. Waiting is what a real
// consumer does, and it is the difference between "not yet" and "never".
func feed(t *testing.T, store *l0.Store, from l0.Cursor, source string, want []string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		// Filtered on the source, not filtered afterwards: the database
		// outlives one test, and a page of somebody else's events would push
		// this one's off the end.
		changes, err := store.Changes(t.Context(), from, l0.Filter{Source: source}, l0.MaxLimit)
		if err != nil {
			t.Fatalf("Changes() = %v, want no error", err)
		}
		got := nativeIDs(eventsOf(changes))
		if slices.Equal(got, want) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// nativeIDs is the native id of each event, which is what every assertion in
// this file compares.
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
