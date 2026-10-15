//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func TestReadsServeStateAndBundleBytesStayIdentical(t *testing.T) {
	w := newWorld(t)
	caller := api.Caller{Principal: "kyle"}
	store := l1.New(w.pool)
	before := w.http(t, caller, "get_bundle", map[string]any{"scope": w.scope})
	label := " In review "
	for _, tt := range []struct {
		name  string
		state *l1.State
	}{
		{"set", &l1.State{Core: connector.CoreStateStarted, Native: &label}},
		{"changed", &l1.State{Core: connector.CoreStateBlocked}},
		{"absent", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, id := range []string{w.issue, w.pr, w.secret} {
				doc, err := store.Get(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				doc.State = tt.state
				if _, err := store.Put(t.Context(), doc.Document); err != nil {
					t.Fatal(err)
				}
				raw := w.http(t, caller, "get_l1", map[string]any{"id": id})
				var got l1.Document
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.State, tt.state) || bytes.Contains(raw, []byte(`"state":`)) != (tt.state != nil) {
					t.Fatalf("get_l1 = %s", raw)
				}
				mcp, failed := w.mcp(t, caller, "get_l1", map[string]any{"id": id})
				if failed || !bytes.Equal(raw, mcp) {
					t.Fatal("MCP state differs")
				}
			}
			raw := w.http(t, caller, "search", map[string]any{"query": "lock", "scope": w.scope})
			var results struct {
				Documents []json.RawMessage `json:"documents"`
			}
			if err := json.Unmarshal(raw, &results); err != nil {
				t.Fatal(err)
			}
			if len(results.Documents) == 0 {
				t.Fatal("search found nothing")
			}
			for _, hit := range results.Documents {
				var doc l1.Document
				if err := json.Unmarshal(hit, &doc); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(doc.State, tt.state) || bytes.Contains(hit, []byte(`"state":`)) != (tt.state != nil) {
					t.Fatalf("search = %s", raw)
				}
			}
			mcp, failed := w.mcp(t, caller, "search", map[string]any{"query": "lock", "scope": w.scope})
			if failed || !bytes.Equal(raw, mcp) {
				t.Fatal("MCP search differs")
			}
			after := w.http(t, caller, "get_bundle", map[string]any{"scope": w.scope})
			if !bytes.Equal(before, after) {
				t.Fatalf("state changed bundle bytes:\nbefore %s\nafter %s", before, after)
			}
		})
	}
}
