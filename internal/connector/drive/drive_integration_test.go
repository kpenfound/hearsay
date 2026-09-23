//go:build integration

package drive_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
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
