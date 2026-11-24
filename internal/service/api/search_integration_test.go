//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/l1"
)

type searchAnswer struct {
	Documents []struct {
		ID    string  `json:"id"`
		Score float64 `json:"score"`
	} `json:"documents"`
	Truncated *bool `json:"truncated"`
}

func (a searchAnswer) ids() []string {
	out := make([]string, len(a.Documents))
	for i, d := range a.Documents {
		out[i] = d.ID
	}
	return out
}

// The search call matches a distillation's own words with no embed tier, applies
// the window, and says whether it was cut off, the same over HTTP and MCP.
func TestSearchFindsDistillationsInAWindowAndReportsTruncation(t *testing.T) {
	w := newWorld(t)
	docs := l1.New(w.pool)

	// issue is at day+1h and pr at day+2h; neither says "heliotrope" in what was
	// said, and the first carries it in its outcome.
	stored, err := docs.Get(t.Context(), w.issue)
	if err != nil {
		t.Fatal(err)
	}
	doc := stored.Document
	doc.RawText = "an unrelated exchange"
	word := "heliotrope" + w.src // the database is shared with every other test
	doc.Body.Outcome = "Adopted the " + word + " plan."
	if _, err := docs.Put(t.Context(), doc); err != nil {
		t.Fatal(err)
	}

	at := func(d time.Duration) string { return day.Add(d).Format(time.RFC3339) }
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
		cut  bool
	}{
		{name: "an outcome-only word", args: map[string]any{"query": word}, want: []string{w.issue}},
		{name: "a window that holds it", args: map[string]any{"query": word, "since": at(0), "until": at(2 * time.Hour)}, want: []string{w.issue}},
		{name: "a window after it", args: map[string]any{"query": word, "since": at(5 * time.Hour)}, want: []string{}},
		{name: "a window before it", args: map[string]any{"query": word, "until": at(0)}, want: []string{}},
		{name: "more matches than the limit", args: map[string]any{"query": "lock", "limit": 1}, want: nil, cut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var viaHTTP, viaMCP searchAnswer
			httpBody := w.http(t, kyle, "search", tc.args)
			mcpBody, isErr := w.mcp(t, kyle, "search", tc.args)
			if isErr {
				t.Fatalf("search over MCP = %s, want a result", mcpBody)
			}
			if string(httpBody) != string(mcpBody) {
				t.Errorf("HTTP answered %s, MCP answered %s, want the same bytes", httpBody, mcpBody)
			}
			if err := json.Unmarshal(httpBody, &viaHTTP); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(mcpBody, &viaMCP)
			if viaHTTP.Truncated == nil {
				t.Fatalf("search answered %s, want truncated always present", httpBody)
			}
			if *viaHTTP.Truncated != tc.cut {
				t.Errorf("truncated = %v, want %v: %s", *viaHTTP.Truncated, tc.cut, httpBody)
			}
			if tc.want != nil && strings.Join(viaHTTP.ids(), ",") != strings.Join(tc.want, ",") {
				t.Errorf("documents = %v, want %v", viaHTTP.ids(), tc.want)
			}
			if tc.want == nil && len(viaHTTP.Documents) != 1 {
				t.Errorf("documents = %v, want one at limit 1", viaHTTP.ids())
			}
		})
	}
}

func TestSearchRefusesABadWindowAs400(t *testing.T) {
	w := newWorld(t)
	for _, tc := range []struct {
		name string
		args string
	}{
		{"a malformed since", `{"query":"lock","since":"last week"}`},
		{"a malformed until", `{"query":"lock","until":"2026-09-09"}`},
		{"an inverted window", `{"query":"lock","since":"2026-09-10T00:00:00Z","until":"2026-09-09T00:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := w.post(t, "/v1/search", kyle, tc.args)
			var refusal struct{ Error, Code string }
			_ = json.Unmarshal(body, &refusal)
			if status != http.StatusBadRequest || refusal.Code != "invalid_request" || refusal.Error == "" {
				t.Errorf("search = %d %s, want 400 invalid_request", status, body)
			}
		})
	}
}
