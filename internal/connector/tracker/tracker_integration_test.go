//go:build integration

package tracker_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/tracker"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

// through serves a fresh source's handler behind the runtime's gate over the
// real L0 store.
func through(t *testing.T, settings map[string]any) (*httptest.Server, *pgxpool.Pool, string) {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	src := source(settings)
	src.ID = "tracker" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c, err := tracker.New(src)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.Handler(connector.NewGate(l0.New(pool), src.ID, c.Describe(), connector.NewAllowlist(src))))
	t.Cleanup(srv.Close)
	return srv, pool, src.ID
}

// stored is every native id L0 holds for a source, tombstoned or not, in the
// order it received them.
func stored(t *testing.T, pool *pgxpool.Pool, source string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT native_id FROM l0_events WHERE source = $1 ORDER BY seq`, source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func eventID(t *testing.T, answer string) string {
	t.Helper()
	var a tracker.Accepted
	if err := json.Unmarshal([]byte(answer), &a); err != nil {
		t.Fatal(err)
	}
	_, native, err := connector.ParseEventID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return native
}

// A sender re-posting everything it holds — the backfill — writes nothing new,
// a late stale revision does not become current, and a deletion posted twice
// is one tombstone.
func TestReplayingWritesNothing(t *testing.T) {
	srv, pool, source := through(t, nil)
	ticket, comment, deletion := fixture(t, "ticket.json"), fixture(t, "comment.json"), fixture(t, "delete.json")
	store := l0.New(pool)

	first := eventID(t, send(t, srv.URL, ticket, http.StatusAccepted))
	remark := eventID(t, send(t, srv.URL, comment, http.StatusAccepted))
	for range 2 {
		send(t, srv.URL, ticket, http.StatusAccepted)
		send(t, srv.URL, with(t, ticket, nil), http.StatusAccepted)
		send(t, srv.URL, comment, http.StatusAccepted)
	}
	if got, want := stored(t, pool, source), []string{first, remark}; !slices.Equal(got, want) {
		t.Fatalf("after replays L0 holds %v, want %v", got, want)
	}

	stale := eventID(t, send(t, srv.URL, with(t, ticket, map[string]any{"title": "Retry", "updated_at": "2026-09-21T00:00:00Z"}), http.StatusAccepted))
	current, ok, err := store.CurrentArtifact(t.Context(), source, "ENG#ENG-42")
	if err != nil || !ok || current.NativeID != first {
		t.Fatalf("after a stale revision the current one is %s (%v, %v), want %s", current.NativeID, ok, err, first)
	}

	send(t, srv.URL, deletion, http.StatusAccepted)
	send(t, srv.URL, deletion, http.StatusNoContent)
	got := stored(t, pool, source)
	if len(got) != 4 || got[2] != stale {
		t.Fatalf("L0 holds %v, want the ticket, the comment, the stale revision and one tombstone", got)
	}
	if _, ok, err := store.CurrentArtifact(t.Context(), source, "ENG#ENG-42"); err != nil || ok {
		t.Fatalf("after the deletion the ticket is current: %v %v", ok, err)
	}
}

// With per-ticket access lists, a comment reads its ticket's through the gate
// from L0.
func TestACommentReadsItsTicketsAccessFromL0(t *testing.T) {
	srv, pool, source := through(t, map[string]any{"ticket_acl": true})
	own := []map[string]string{{"kind": "identity", "native_id": "u-17"}}
	send(t, srv.URL, fixture(t, "comment.json"), http.StatusConflict)
	send(t, srv.URL, with(t, fixture(t, "ticket.json"), map[string]any{"acl": own}), http.StatusAccepted)
	send(t, srv.URL, fixture(t, "comment.json"), http.StatusAccepted)

	ev, ok, err := l0.New(pool).CurrentArtifact(t.Context(), source, "ENG#ENG-42:comment:9001")
	if err != nil || !ok {
		t.Fatalf("the comment is not held: %v %v", ok, err)
	}
	want := connector.ACL{{Kind: connector.ACLIdentity, Source: source, NativeID: "u-17"}}
	if !reflect.DeepEqual(ev.ACL, want) {
		t.Fatalf("the comment's acl is %v, want its ticket's %v", ev.ACL, want)
	}
}
