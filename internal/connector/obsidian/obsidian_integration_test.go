//go:build integration

package obsidian_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/obsidian"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

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
