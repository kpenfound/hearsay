//go:build integration

package agent_test

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
	"github.com/kpenfound/hearsay/internal/connector/agent"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

// Through the real store: a replayed event writes nothing, and the same event
// key posted with other content is refused rather than rewritten.
func TestASessionLandsInL0Once(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	src := source()
	src.ID = "agent" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c, err := agent.New(src, principals, lookup)
	if err != nil {
		t.Fatal(err)
	}
	store := l0.New(pool)
	srv := httptest.NewServer(c.Handler(connector.NewGate(store, src.ID, c.Describe(), connector.NewAllowlist(src))))
	defer srv.Close()

	tests := []struct {
		name string
		body string
		want int
	}{
		{"start", start, http.StatusAccepted},
		{"a turn", turn, http.StatusAccepted},
		{"a tool call", call, http.StatusAccepted},
		{"end", end, http.StatusAccepted},
		{"the turn again", turn, http.StatusAccepted},
		{"the tool call again", call, http.StatusAccepted},
		{"the turn's key with other text", strings.Replace(turn, "Reading", "Rewriting", 1), http.StatusConflict},
	}
	for _, tt := range tests {
		if code, body := post(t, srv.URL, env["SHED_TOKEN"], tt.body); code != tt.want {
			t.Errorf("%s: POST = %d %s, want %d", tt.name, code, body, tt.want)
		}
	}

	// The artifact's history is the session, in the order it happened.
	history, err := store.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Artifact: "s-01"}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range history {
		got = append(got, ev.NativeID)
	}
	if want := []string{"s-01@start", "s-01@turn:1", "s-01@call:c-1", "s-01@end"}; !slices.Equal(got, want) {
		t.Fatalf("L0 holds %v, want the session's four events once each, in order: %v", got, want)
	}
	for _, ev := range history {
		if ev.Kind == connector.KindAgentTurn && !strings.HasPrefix(ev.Payload.Text, "Reading") {
			t.Errorf("the turn was rewritten to %q", ev.Payload.Text)
		}
	}
}
