//go:build integration

package drive_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/drive"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

// The SQL cursor and sink survive a connector replacement, and the real gate
// only lets the three directly allowlisted, label-eligible files reach L0.
func TestDriveBackfillThroughGateAndSQL(t *testing.T) {
	database := os.Getenv("HEARSAY_DATABASE_URL")
	if database == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &fixture{}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := source(t, server.URL)
	src.ID = "drive" + strconv.FormatInt(time.Now().UnixNano(), 36)
	events := l0.New(pool)
	cursors := l0.NewBackfillCursors(pool)
	c, err := drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	gate := connector.NewGate(events, src.ID, c.Describe(), connector.NewAllowlist(src))
	first, err := c.Backfill(t.Context(), gate, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Done {
		t.Fatal("first page unexpectedly finished")
	}
	if err := cursors.Save(t.Context(), src.ID, connector.BackfillState{Cursor: first.Next, Events: int64(first.Events)}); err != nil {
		t.Fatal(err)
	}
	c, err = drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	gate = connector.NewGate(events, src.ID, c.Describe(), connector.NewAllowlist(src))
	state, err := cursors.Load(t.Context(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	for !state.Done {
		result, err := c.Backfill(t.Context(), gate, state.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		state.Cursor = result.Next
		state.Done = result.Done
		state.Events += int64(result.Events)
		if err := cursors.Save(t.Context(), src.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, ev := range stored {
		ids = append(ids, ev.NativeID)
	}
	slices.Sort(ids)
	if want := []string{"doc1@r1", "doc2@r2", "tagged@head"}; !slices.Equal(ids, want) {
		t.Fatalf("stored native IDs = %v, want %v", ids, want)
	}
	// A repeated page is harmless in the actual L0 sink.
	if _, err := c.Backfill(t.Context(), gate, ""); err != nil {
		t.Fatal(err)
	}
	again, err := events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(stored) {
		t.Errorf("replayed page wrote %d events, want %d", len(again), len(stored))
	}
}

// The change cursor, event IDs, current revision and retraction all survive a
// connector replacement through the real SQL sink.
func TestDriveLiveSyncThroughSQL(t *testing.T) {
	database := os.Getenv("HEARSAY_DATABASE_URL")
	if database == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &liveFixture{files: map[string]liveFile{
		"doc":      {Folder: "docs", Head: "r1", Text: "one", Public: true},
		"untagged": {Folder: "meet", Head: "r1", Text: "hidden", Public: true},
		"outside":  {Folder: "elsewhere", Head: "r1", Text: "hidden", Public: true},
	}, changes: map[string][]string{"s0": {}}, start: "s0"}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := source(t, server.URL)
	src.ID = "drivelive" + strconv.FormatInt(time.Now().UnixNano(), 36)
	events := l0.New(pool)
	cursors := l0.NewBackfillCursors(pool)
	c, err := drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	gate := connector.NewGate(events, src.ID, c.Describe(), connector.NewAllowlist(src))
	cursor, err := c.PollFrom(t.Context(), gate, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cursors.Save(t.Context(), src.ID, connector.BackfillState{Cursor: cursor}); err != nil {
		t.Fatal(err)
	}
	current := func() []connector.Event {
		t.Helper()
		got, err := events.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Kind: connector.KindDocument}, Limit: l0.MaxLimit})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := current(); len(got) != 1 || got[0].Payload.Artifact != "doc" {
		t.Fatalf("initial current = %+v", got)
	}
	// Restart and resume from the SQL cursor after a sharing-only change.
	c, err = drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	f.set("doc", liveFile{Folder: "docs", Head: "r1", Text: "one", Public: false})
	f.next("s0", "s1", "doc", "untagged", "outside")
	state, err := cursors.Load(t.Context(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := c.PollFrom(t.Context(), gate, state.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if err := cursors.Save(t.Context(), src.ID, connector.BackfillState{Cursor: next}); err != nil {
		t.Fatal(err)
	}
	got := current()
	if len(got) != 1 || got[0].Payload.Text != "one" || !strings.HasPrefix(got[0].Payload.Revision.Token, "r1+perm:") {
		t.Fatalf("ACL-only revision = %+v", got)
	}
	for _, entry := range got[0].ACL {
		if entry.Kind == connector.ACLPublic {
			t.Fatal("current ACL retained public access")
		}
	}
	before, err := events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollFrom(t.Context(), gate, state.Cursor); err != nil {
		t.Fatal(err)
	}
	after, err := events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("replay wrote %d new rows", len(after)-len(before))
	}
	// A move outside the allowlist retracts only the formerly eligible file.
	f.set("doc", liveFile{Folder: "elsewhere", Head: "r1", Text: "one", Public: false})
	f.next("s1", "s2", "doc")
	next, err = c.PollFrom(t.Context(), gate, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := current(); len(got) != 0 {
		t.Errorf("moved-out file remains current: %+v", got)
	}
	tombs, err := events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Kind: connector.KindTombstone}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(tombs) != 1 || tombs[0].Payload.Target != "doc" || tombs[0].Payload.Container.NativeID != "docs" || len(tombs[0].ACL) != len(after[len(after)-1].ACL) {
		t.Errorf("tombstone = %+v", tombs)
	}
	firstTomb := tombs[0]
	// A move back restores the same artifact with fresh content and ACL.
	f.set("doc", liveFile{Folder: "docs", Head: "r2", Text: "returned", Public: true})
	f.next("s2", "s3", "doc")
	next, err = c.PollFrom(t.Context(), gate, next)
	if err != nil {
		t.Fatal(err)
	}
	got = current()
	if len(got) != 1 || got[0].Payload.Artifact != "doc" || got[0].Payload.Text != "returned" || !slices.ContainsFunc(got[0].ACL, func(a connector.ACLEntry) bool { return a.Kind == connector.ACLPublic }) {
		t.Fatalf("restored document = %+v", got)
	}
	if _, err := events.Get(t.Context(), firstTomb.ID); err != nil {
		t.Errorf("original tombstone lost: %v", err)
	}
	// Replaying the old move-out page observes the present file and cannot
	// retract its new revision.
	if _, err := c.PollFrom(t.Context(), gate, "s1"); err != nil {
		t.Fatal(err)
	}
	if got := current(); len(got) != 1 || got[0].Payload.Text != "returned" {
		t.Errorf("old retraction replay hid document: %+v", got)
	}
	// A second move-out has its own tombstone, and a move back with the
	// same Drive head still gets a distinct eligible revision.
	f.set("doc", liveFile{Folder: "elsewhere", Head: "r2", Text: "returned", Public: true})
	f.next("s3", "s3a", "doc")
	next, err = c.PollFrom(t.Context(), gate, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := current(); len(got) != 0 {
		t.Fatalf("second move-out current = %+v", got)
	}
	f.set("doc", liveFile{Folder: "docs", Head: "r2", Text: "returned", Public: true})
	f.next("s3a", "s3b", "doc")
	next, err = c.PollFrom(t.Context(), gate, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := current(); len(got) != 1 || got[0].Payload.Text != "returned" {
		t.Fatalf("second move-back current = %+v", got)
	}
	docTombs, err := events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Kind: connector.KindTombstone}})
	if err != nil || len(docTombs) != 2 || docTombs[0].ID == docTombs[1].ID {
		t.Fatalf("document tombstone cycles = %+v, %v", docTombs, err)
	}
	// A tagged transcript enters, leaves when untagged, then returns through
	// expired-token reconciliation after a connector restart.
	f.set("meeting", liveFile{Folder: "meet", Head: "t1", Text: "initial", Tagged: true, Public: true})
	f.next("s3b", "s4", "meeting")
	next, err = c.PollFrom(t.Context(), gate, next)
	if err != nil {
		t.Fatal(err)
	}
	transcripts := func() []connector.Event {
		t.Helper()
		rows, err := events.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Kind: connector.KindTranscript}})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if got := transcripts(); len(got) != 1 {
		t.Fatalf("initial transcript = %+v", got)
	}
	f.set("meeting", liveFile{Folder: "meet", Head: "t1", Text: "initial", Tagged: false, Public: true})
	f.next("s4", "s5", "meeting")
	next, err = c.PollFrom(t.Context(), gate, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := transcripts(); len(got) != 0 {
		t.Fatalf("untagged transcript current = %+v", got)
	}
	tombs, err = events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Kind: connector.KindTombstone}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tombs) != 3 {
		t.Fatalf("tombstone history = %+v", tombs)
	}
	f.set("meeting", liveFile{Folder: "meet", Head: "t1", Text: "wrong tag", LabelID: "other", Public: true})
	f.next("s5", "s5b", "meeting")
	next, err = c.PollFrom(t.Context(), gate, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := transcripts(); len(got) != 0 {
		t.Errorf("wrongly tagged transcript current = %+v", got)
	}
	f.set("meeting", liveFile{Folder: "meet", Head: "t1", Text: "unreadable tag", UnreadableLabels: true, Public: true})
	f.next("s5b", "s5c", "meeting")
	next, err = c.PollFrom(t.Context(), gate, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := transcripts(); len(got) != 0 {
		t.Errorf("unreadable transcript current = %+v", got)
	}
	f.set("meeting", liveFile{Folder: "meet", Head: "t2", Text: "retagged", Tagged: true, Public: false})
	f.next("s5c", "s6", "meeting")
	c, err = drive.New(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollFrom(t.Context(), gate, "expired"); err != nil {
		t.Fatal(err)
	}
	got = transcripts()
	if len(got) != 1 || got[0].Payload.Artifact != "meeting" || got[0].Payload.Text != "retagged" || slices.ContainsFunc(got[0].ACL, func(a connector.ACLEntry) bool { return a.Kind == connector.ACLPublic }) {
		t.Fatalf("restored transcript = %+v", got)
	}
	visibleBefore, err := events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Artifact: "meeting"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollFrom(t.Context(), gate, next); err != nil {
		t.Fatal(err)
	}
	visibleAfter, err := events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Artifact: "meeting"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(visibleAfter) != len(visibleBefore) || len(transcripts()) != 1 {
		t.Errorf("replay changed transcript: before %+v, after %+v", visibleBefore, visibleAfter)
	}
	for _, tomb := range tombs {
		if _, err := events.Get(t.Context(), tomb.ID); err != nil {
			t.Errorf("tombstone %s lost: %v", tomb.ID, err)
		}
	}
}
