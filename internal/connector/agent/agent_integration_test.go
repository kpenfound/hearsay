//go:build integration

package agent_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/agent"
	"github.com/kpenfound/hearsay/internal/connector/connectortest"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
)

func TestASessionLandsInL0Once(t *testing.T) {
	pool := dbtest.Pool(t)

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
		{"a next action", next, http.StatusAccepted},
		{"end", end, http.StatusAccepted},
		{"the turn again", turn, http.StatusAccepted},
		{"the tool call again", call, http.StatusAccepted},
		{"the next action again", next, http.StatusAccepted},
		{"the turn's key with other text", strings.Replace(turn, "Reading", "Rewriting", 1), http.StatusConflict},
		{"the next action's key with another verdict", strings.Replace(next, "real", "spurious", 1), http.StatusConflict},
	}
	for _, tt := range tests {
		if code, body := connectortest.PostWithToken(t, srv.URL, env["SHED_TOKEN"], tt.body); code != tt.want {
			t.Errorf("%s: POST = %d %s, want %d", tt.name, code, body, tt.want)
		}
	}

	history, err := store.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src.ID, Artifact: "s-01"}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range history {
		got = append(got, ev.NativeID)
	}
	if want := []string{"s-01@start", "s-01@turn:1", "s-01@call:c-1", "s-01@next:1", "s-01@end"}; !slices.Equal(got, want) {
		t.Fatalf("L0 holds %v, want the session's five events once each, in order: %v", got, want)
	}
	for _, ev := range history {
		if ev.Kind == connector.KindAgentTurn && !strings.HasPrefix(ev.Payload.Text, "Reading") {
			t.Errorf("the turn was rewritten to %q", ev.Payload.Text)
		}
	}
}
