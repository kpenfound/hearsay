//go:build integration

package obsidian_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/obsidian"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

func TestPollReconcilesVaultThroughL0(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	root := t.TempDir()
	put(t, root, "Notes/edit.md", "first")
	put(t, root, "Notes/rename.md", "rename")
	put(t, root, "Notes/delete.md", "delete")
	put(t, root, "Projects/leave.md", "leave")
	put(t, root, "Notes/Templates/blank.md", "excluded")
	put(t, root, "Other/out.md", "excluded")
	outside := t.TempDir()
	put(t, outside, "escape.md", "escape")
	if err := os.Symlink(outside, filepath.Join(root, "Notes", "Link")); err != nil {
		t.Fatal(err)
	}
	src := source(root)
	src.ID = "obsidianpoll" + strconv.FormatInt(time.Now().UnixNano(), 36)
	src.Containers = []string{"Notes", "Projects"}
	store := l0.New(pool)
	connect := func() *obsidian.Connector {
		t.Helper()
		c, err := obsidian.New(src)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := connect()
	sink := connector.NewGate(store, src.ID, c.Describe(), connector.NewAllowlist(src))
	walk(t, c, sink, "")
	check := func(want int) []connector.Event {
		t.Helper()
		got, err := store.Documents(t.Context(), src.ID)
		if err != nil || len(got) != want {
			t.Fatalf("Documents() = %d, %v, want %d", len(got), err, want)
		}
		return got
	}
	check(4)
	if err := c.Poll(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	before, err := store.Changes(t.Context(), l0.Cursor{}, l0.Filter{Source: src.ID}, l0.MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "Notes/edit.md", "second")
	changed := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "Notes", "edit.md"), changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "Notes", "rename.md"), filepath.Join(root, "Notes", "renamed.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "Notes", "delete.md")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	src.Containers = []string{"Notes"} // a config restart removes Projects
	c = connect()
	defer func() { _ = c.Close(t.Context()) }()
	sink = connector.NewGate(store, src.ID, c.Describe(), connector.NewAllowlist(src))
	if err := c.Poll(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	got := check(2)
	artifacts := map[string]bool{}
	for _, ev := range got {
		artifacts[ev.Payload.Artifact] = true
		if ev.Payload.Artifact == "Notes/edit.md" && (ev.Payload.Text != "second" || ev.ACL[0].Kind != connector.ACLIdentity) {
			t.Fatalf("edited current event = %+v", ev)
		}
	}
	if !artifacts["Notes/edit.md"] || !artifacts["Notes/renamed.md"] {
		t.Fatalf("current artifacts = %v", artifacts)
	}
	after, err := store.Changes(t.Context(), l0.Cursor{}, l0.Filter{Source: src.ID}, l0.MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	// The change feed hides document rows once a tombstone retracts them.
	if len(after) != len(before)+2 {
		t.Fatalf("visible events after reconciliation = %d, want %d", len(after), len(before)+2)
	}
	targets := map[string]bool{}
	for _, change := range after {
		if change.Event.Kind == connector.KindTombstone {
			targets[change.Event.Payload.Target] = true
		}
	}
	for _, target := range []string{"Notes/rename.md", "Notes/delete.md", "Projects/leave.md"} {
		if !targets[target] {
			t.Fatalf("missing tombstone for %q: %v", target, targets)
		}
	}
	if err := c.Poll(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	again, err := store.Changes(t.Context(), l0.Cursor{}, l0.Filter{Source: src.ID}, l0.MaxLimit)
	if err != nil || len(again) != len(after) {
		t.Fatalf("unchanged poll events = %d, %v", len(again), err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	var settings obsidian.Settings
	if err := json.Unmarshal(src.Settings, &settings); err != nil {
		t.Fatal(err)
	}
	settings.Public = true
	src.Settings, _ = json.Marshal(settings)
	c = connect()
	sink = connector.NewGate(store, src.ID, c.Describe(), connector.NewAllowlist(src))
	if err := c.Poll(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	put(t, root, "Notes/edit.md", "third")
	changed = changed.Add(3 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "Notes", "edit.md"), changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := c.Poll(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
	for _, ev := range check(2) {
		if len(ev.ACL) != 1 || ev.ACL[0].Kind != connector.ACLPublic {
			t.Fatalf("public poll ACL = %+v", ev.ACL)
		}
	}
}

func TestBackfillThroughGateAndSQL(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	root := t.TempDir()
	for i := 0; i < 120; i++ {
		put(t, root, fmt.Sprintf("Notes/n%03d.md", i), "# note")
	}
	put(t, root, "Other/excluded.md", "excluded")
	src := source(root)
	src.ID = "obsidian" + strconv.FormatInt(time.Now().UnixNano(), 36)
	sink := l0.New(pool)
	cursors := l0.NewBackfillCursors(pool)
	c, err := obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	denied := connector.NewGate(sink, src.ID, c.Describe(), connector.NewAllowlist())
	_, err = c.Backfill(t.Context(), denied, "")
	if err != nil {
		t.Fatal(err)
	}
	if denied.Dropped() == 0 {
		t.Fatal("unconfigured gate accepted notes")
	}
	first, err := c.Backfill(t.Context(), connector.NewGate(sink, src.ID, c.Describe(), connector.NewAllowlist(src)), "")
	if err != nil || first.Done || first.Next == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	if err := cursors.Save(t.Context(), src.ID, connector.BackfillState{Cursor: first.Next, Events: int64(first.Events)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, err = obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	state, err := cursors.Load(t.Context(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	for !state.Done {
		result, err := c.Backfill(t.Context(), connector.NewGate(sink, src.ID, c.Describe(), connector.NewAllowlist(src)), state.Cursor)
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
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := sink.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 120 {
		t.Fatalf("current notes = %d, want 120", len(stored))
	}
	for _, ev := range stored {
		if len(ev.ACL) != 1 || ev.ACL[0].Kind != connector.ACLIdentity || ev.ACL[0].NativeID != "owner-1" {
			t.Fatalf("private ACL = %+v", ev.ACL)
		}
	}
	var settings obsidian.Settings
	if err := json.Unmarshal(src.Settings, &settings); err != nil {
		t.Fatal(err)
	}
	settings.Public = true
	src.Settings, _ = json.Marshal(settings)
	c, err = obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	cursor := connector.Cursor("")
	for {
		result, err := c.Backfill(t.Context(), connector.NewGate(sink, src.ID, c.Describe(), connector.NewAllowlist(src)), cursor)
		if err != nil {
			t.Fatal(err)
		}
		if result.Done {
			break
		}
		cursor = result.Next
	}
	current, err := sink.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 120 {
		t.Fatalf("public current notes = %d", len(current))
	}
	for _, ev := range current {
		if len(ev.ACL) != 1 || ev.ACL[0].Kind != connector.ACLPublic {
			t.Fatalf("current ACL = %+v", ev.ACL)
		}
	}
}
