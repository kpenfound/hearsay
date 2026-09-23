//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func TestAssertionTracesToSessionAndBundles(t *testing.T) {
	w := newWorld(t)
	source, session := w.src+"session", "session-1"
	started := time.Now().UTC().Add(-time.Hour)
	for _, step := range []struct {
		kind           connector.Kind
		revision, text string
		at             time.Time
	}{
		{connector.KindAgentSession, "start", "", started},
		{connector.KindAgentTurn, "turn:1", "Read the bundle", started.Add(time.Second)},
		{connector.KindToolCall, "call:1", "", started.Add(2 * time.Second)},
	} {
		author := connector.Identity{Source: source, Kind: connector.IdentityAgent, NativeID: "shed"}
		ev := connector.Event{
			Source: source, NativeID: session + "@" + step.revision, Kind: step.kind, Time: started,
			Payload: connector.Payload{
				Artifact: session, Container: connector.Container{Kind: "stream", NativeID: "shed"},
				Author: &author, Participants: []connector.Participant{{Identity: connector.Identity{Source: source, Kind: connector.IdentityUser, NativeID: "kyle"}, Role: connector.RoleAuthor}},
				Text: step.text, Revision: &connector.Revision{Token: step.revision, EditedAt: step.at},
				Native: json.RawMessage(`{"session":"session-1"}`),
			},
			ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: source, NativeID: "shed"}, {Kind: connector.ACLIdentity, Source: source, NativeID: "kyle"}},
		}
		if _, err := l0.New(w.pool).Append(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	withSession := api.Caller{Principal: "kyle", Agent: "shed", Session: session}
	bundleArgs := map[string]any{"scope": w.scope}
	bundleHTTP := w.http(t, withSession, "get_bundle", bundleArgs)
	bundleMCP, bundleError := w.mcp(t, withSession, "get_bundle", bundleArgs)
	if bundleError || !bytes.Equal(bundleHTTP, bundleMCP) {
		t.Fatalf("session bundle differs over HTTP and MCP")
	}
	var asserted api.Asserted
	assertArgs := map[string]any{
		"topic": w.lockTopic(), "position": "The lock moves to the worker", "evidence": []string{w.issue},
	}
	assertHTTP := w.http(t, withSession, "assert", assertArgs)
	assertMCP, assertError := w.mcp(t, withSession, "assert", assertArgs)
	if assertError || !bytes.Equal(assertHTTP, assertMCP) {
		t.Fatalf("session assertion differs over HTTP and MCP")
	}
	if err := json.Unmarshal(assertHTTP, &asserted); err != nil {
		t.Fatal(err)
	}
	event, err := l0.New(w.pool).Get(t.Context(), asserted.ID)
	if err != nil {
		t.Fatal(err)
	}
	as, err := l2.AssertionOf(event)
	if err != nil || as.Session != session || as.SessionSource != source {
		t.Fatalf("assertion session = %+v, %v", as, err)
	}
	args := map[string]any{"assertion": asserted.ID}
	httpBody := w.http(t, kyle, "get_session", args)
	mcpBody, isError := w.mcp(t, kyle, "get_session", args)
	if isError || !bytes.Equal(httpBody, mcpBody) {
		t.Fatalf("HTTP trace = %s; MCP trace = %s (error %v)", httpBody, mcpBody, isError)
	}
	var trace api.SessionTrace
	if err := json.Unmarshal(httpBody, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.Source != source || trace.Artifact != session || len(trace.Events) != 5 {
		t.Fatalf("trace = %+v", trace)
	}
	for i, kind := range []connector.Kind{connector.KindAgentSession, connector.KindAgentTurn, connector.KindToolCall, connector.KindAudit, connector.KindAudit} {
		if trace.Events[i].Kind != kind {
			t.Errorf("event %d kind = %s, want %s", i, trace.Events[i].Kind, kind)
		}
	}
	for _, ev := range trace.Events[3:] {
		var audit api.AuditRecord
		if err := json.Unmarshal(ev.Payload.Native, &audit); err != nil || audit.Session != session || audit.SessionSource != source {
			t.Errorf("audit = %+v, %v", audit, err)
		}
	}
	if status, _ := w.post(t, "/v1/get_session", sam, string(mustJSON(t, args))); status != http.StatusNotFound {
		t.Errorf("Sam follows Kyle's session: %d", status)
	}
	if status, _ := w.post(t, "/v1/get_session", peekKyle, string(mustJSON(t, args))); status != http.StatusNotFound {
		t.Errorf("another agent follows Shed's session: %d", status)
	}
	if status, _ := w.post(t, "/v1/get_bundle", api.Caller{Principal: "kyle", Session: session}, string(mustJSON(t, map[string]any{"scope": w.scope}))); status != http.StatusForbidden {
		t.Errorf("human-only session call = %d, want 403", status)
	}
	without := w.http(t, shedKyle, "get_bundle", map[string]any{"scope": w.scope})
	if len(without) == 0 {
		t.Fatal("bundle without a session is empty")
	}
	var ordinary api.Asserted
	if err := json.Unmarshal(w.http(t, shedKyle, "assert", map[string]any{
		"topic": w.lockTopic(), "position": "An assertion outside the session", "evidence": []string{w.issue},
	}), &ordinary); err != nil {
		t.Fatal(err)
	}
	ordinaryEvent, err := l0.New(w.pool).Get(t.Context(), ordinary.ID)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryAssertion, err := l2.AssertionOf(ordinaryEvent)
	if err != nil || ordinaryAssertion.Session != "" || ordinaryAssertion.SessionSource != "" {
		t.Errorf("assertion without header = %+v, %v", ordinaryAssertion, err)
	}
	if status, _ := w.post(t, "/v1/get_session", kyle, string(mustJSON(t, map[string]any{"assertion": ordinary.ID}))); status != http.StatusNotFound {
		t.Errorf("unlinked assertion has a session: %d", status)
	}
	audits, err := l0.New(w.pool).List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: api.AuditSource, Kind: connector.KindAudit}, Newest: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var unlinked bool
	for _, ev := range audits {
		var rec api.AuditRecord
		if err := json.Unmarshal(ev.Payload.Native, &rec); err != nil {
			t.Fatal(err)
		}
		unlinked = unlinked || rec.Scope == w.scope && rec.Session == "" && rec.SessionSource == ""
	}
	if !unlinked {
		t.Fatal("bundle served without the header has no unlinked audit")
	}
	if _, err := l1.New(w.pool).Delete(t.Context(), w.issue); err != nil {
		t.Fatal(err)
	}
	if status, _ := w.post(t, "/v1/get_session", kyle, string(mustJSON(t, args))); status != http.StatusNotFound {
		t.Errorf("trace after its assertion evidence was retracted = %d, want 404", status)
	}
}
