//go:build integration

package l0_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
)

const secret = "tangerine-otter-42"

// The acceptance criteria at L0: an operator deletion redacts the row in
// place, every read hides it, a read by id names the deletion rather than a
// tombstone, a replay is dropped and counted, and a new revision is admitted.
func TestAnOperatorDeletionRedactsTheRowAndHidesIt(t *testing.T) {
	pool := newPool(t)
	fake, source := newFake(t)
	store := l0.New(pool)
	ctx := t.Context()

	root := fake.NewEvent(connector.KindMessage, "root", "the thread")
	pasted := fake.NewEvent(connector.KindMessage, "m1", "the vault passphrase is "+secret)
	pasted.Payload.Thread, pasted.Payload.Parent = "root", "root"
	kept := fake.NewEvent(connector.KindMessage, "m2", "please rotate it")
	kept.Payload.Thread, kept.Payload.Parent = "root", "root"
	for _, ev := range []connector.Event{root, pasted, kept} {
		if _, err := store.Append(ctx, ev); err != nil {
			t.Fatalf("Append(%s) = %v", ev.NativeID, err)
		}
	}
	pastedID := connector.EventID(source, "m1")
	var before struct {
		xact, kind, acl string
		seq             int64
		at              time.Time
	}
	if err := pool.QueryRow(ctx, `SELECT xact_id::text, seq, kind, occurred_at, acl::text FROM l0_events WHERE id = $1`, pastedID).
		Scan(&before.xact, &before.seq, &before.kind, &before.at, &before.acl); err != nil {
		t.Fatal(err)
	}

	id := "del_" + source
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := l0.Delete(ctx, tx, l0.Deletion{
		ID: id, Operator: "kyle", Reason: "a pasted secret", Selector: json.RawMessage(`{"event":"` + pastedID + `"}`),
		Events: []string{pastedID}, Documents: []string{"l1:" + source + ":root"}, Time: time.Now(),
	})
	if err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted.Events, []string{pastedID}) {
		t.Errorf("Delete().Events = %v, want %v", deleted.Events, []string{pastedID})
	}

	// The row is still there, in the same place, and says nothing it said.
	var after struct {
		xact, kind, acl, payload string
		seq                      int64
		at                       time.Time
		deletion                 *string
	}
	if err := pool.QueryRow(ctx, `SELECT xact_id::text, seq, kind, occurred_at, acl::text, payload::text, deletion FROM l0_events WHERE id = $1`, pastedID).
		Scan(&after.xact, &after.seq, &after.kind, &after.at, &after.acl, &after.payload, &after.deletion); err != nil {
		t.Fatal(err)
	}
	if after.xact != before.xact || after.seq != before.seq || after.kind != before.kind || !after.at.Equal(before.at) || after.acl != before.acl {
		t.Errorf("the redacted row moved or changed kind, time or ACL: before %+v, after %+v", before, after)
	}
	if after.deletion == nil || *after.deletion != id {
		t.Errorf("deletion column = %v, want %s", after.deletion, id)
	}
	for _, gone := range []string{secret, "someone", `"author"`, `"text"`} {
		if strings.Contains(after.payload, gone) {
			t.Errorf("the redacted payload still holds %q: %s", gone, after.payload)
		}
	}
	for _, kept := range []string{`"artifact": "m1"`, `"thread": "root"`, `"redacted": {"deletion": "` + id + `"}`} {
		if !strings.Contains(after.payload, kept) {
			t.Errorf("the redacted payload lost %s: %s", kept, after.payload)
		}
	}

	// A read by id names the deletion, and it is not a tombstone.
	_, err = store.Get(ctx, pastedID)
	var gone *l0.DeletedError
	if !errors.As(err, &gone) || !errors.Is(err, l0.ErrDeleted) || errors.Is(err, l0.ErrRetracted) {
		t.Fatalf("Get(deleted) = %v, want a DeletedError that is not ErrRetracted", err)
	}
	if gone.Deletion != id || gone.Operator != "kyle" || strings.Contains(err.Error(), secret) {
		t.Errorf("Get(deleted) = %+v (%v), want deletion %s by kyle and no content", gone, err, id)
	}

	// Every other read leaves it out.
	listed, err := store.List(ctx, l0.ListOptions{Filter: l0.Filter{Source: source}})
	if err != nil || !slices.Equal(nativeIDs(listed), []string{"root", "m2"}) {
		t.Errorf("List() = %v, %v, want root and m2", nativeIDs(listed), err)
	}
	thread, err := store.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: source, Thread: "root"}})
	if err != nil || !slices.Equal(nativeIDs(thread), []string{"m2"}) {
		t.Errorf("Current(thread) = %v, %v, want m2", nativeIDs(thread), err)
	}
	if got := feed(t, store, l0.Cursor{}, source, []string{"root", "m2"}); !slices.Equal(got, []string{"root", "m2"}) {
		t.Errorf("the feed = %v, want root and m2", got)
	}
	assertCounts(t, store, source, connector.KindMessage, 3, 2)

	// Hearsay's own event records it, under its own source and kind.
	retraction, err := store.Get(ctx, deleted.Retraction)
	if err != nil {
		t.Fatalf("Get(retraction) = %v", err)
	}
	if retraction.Source != connector.SelfSource || retraction.Kind != connector.KindDeletion || retraction.Payload.Target != "" ||
		retraction.Payload.Author == nil || retraction.Payload.Author.NativeID != "kyle" || !strings.Contains(string(retraction.Payload.Native), id) {
		t.Errorf("the retraction event = %+v, want a hearsay deletion by kyle naming %s", retraction, id)
	}

	// A replay is dropped, counted and not an error, however it arrives.
	replay, err := store.Append(ctx, pasted)
	if err != nil || replay.Stored || replay.Dropped != id {
		t.Errorf("Append(replay) = %+v, %v, want dropped by %s", replay, err, id)
	}
	if err := store.Emit(ctx, pasted); err != nil {
		t.Errorf("Emit(replay) = %v, want nil: a replay must not stall a connector", err)
	}
	var replays int64
	if err := pool.QueryRow(ctx, `SELECT replays FROM l0_deletions WHERE id = $1`, id).Scan(&replays); err != nil || replays != 2 {
		t.Errorf("replays = %d, %v, want 2", replays, err)
	}
	if _, err := store.Get(ctx, pastedID); !errors.Is(err, l0.ErrDeleted) {
		t.Errorf("Get(after replay) = %v, want still deleted", err)
	}

	// A new revision is a new row, and it is admitted.
	edited := pasted
	edited.NativeID = "m1@2"
	edited.Payload.Revision = &connector.Revision{Token: "2", EditedAt: pasted.Time.Add(time.Hour)}
	edited.Payload.Text = "(removed)"
	if got, err := store.Append(ctx, edited); err != nil || !got.Stored || got.Dropped != "" {
		t.Errorf("Append(new revision) = %+v, %v, want stored", got, err)
	}
	current, ok, err := store.CurrentArtifact(ctx, source, "m1")
	if err != nil || !ok || current.NativeID != "m1@2" {
		t.Errorf("CurrentArtifact(m1) = %s, %v, %v, want the new revision", current.NativeID, ok, err)
	}

	// Deleting what is already deleted does nothing.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := l0.Delete(ctx, tx, l0.Deletion{
		ID: id + "_again", Operator: "kyle", Reason: "again", Selector: json.RawMessage(`{}`),
		Events: []string{pastedID, deleted.Retraction}, Time: time.Now(),
	}); !errors.Is(err, l0.ErrNothingToDelete) {
		t.Errorf("Delete(already deleted, and a deletion event) = %v, want ErrNothingToDelete", err)
	}
}

// A source tombstone keeps the source's attribution, and is never reported as
// an operator deletion.
func TestATombstonedEventIsAttributedToItsSource(t *testing.T) {
	store, fake, source := newStore(t)
	ctx := t.Context()
	event := fake.NewEvent(connector.KindMessage, "m1", "hello")
	tombstone := fake.NewEvent(connector.KindTombstone, "m1:tombstone", "")
	tombstone.Payload.Target = "m1"
	for _, ev := range []connector.Event{event, tombstone} {
		if _, err := store.Append(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.Get(ctx, connector.EventID(source, "m1"))
	var retracted *l0.RetractedError
	if !errors.As(err, &retracted) || errors.Is(err, l0.ErrDeleted) {
		t.Fatalf("Get(tombstoned) = %v, want a RetractedError that is not ErrDeleted", err)
	}
	if retracted.Source != source || retracted.Tombstone != connector.EventID(source, "m1:tombstone") {
		t.Errorf("Get(tombstoned) = %+v, want source %s and its tombstone", retracted, source)
	}
}
