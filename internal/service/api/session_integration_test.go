//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/bundle"
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
	if _, err := l2.New(w.pool).Pin(t.Context(), l2.Pin{Scope: w.scope, L1: w.pr, PinnedBy: "kyle", PinnedAt: started}); err != nil {
		t.Fatal(err)
	}
	bundleArgs := map[string]any{"scope": w.scope}
	bundleHTTP := w.http(t, withSession, "get_bundle", bundleArgs)
	bundleMCP, bundleError := w.mcp(t, withSession, "get_bundle", bundleArgs)
	if bundleError || !bytes.Equal(bundleHTTP, bundleMCP) {
		t.Fatalf("session bundle differs over HTTP and MCP")
	}
	var served bundle.Bundle
	if err := json.Unmarshal(bundleHTTP, &served); err != nil {
		t.Fatal(err)
	}
	if len(served.Anchors) == 0 || len(served.Stances) == 0 {
		t.Fatalf("fixture has no handles: %+v", served)
	}
	w.http(t, withSession, "get_l1", map[string]any{"id": served.Anchors[0].L1})
	w.http(t, withSession, "stance_history", map[string]any{"topic": served.Stances[0].TopicID})
	w.http(t, withSession, "search", map[string]any{"query": "lock", "scope": w.scope})
	w.http(t, withSession, "resolve", map[string]any{"text": w.project})
	w.http(t, withSession, "get_l0", map[string]any{"id": connector.EventID(w.src, w.project+"#12")})
	for _, name := range []string{"get_l1", "get_l0", "stance_history"} {
		key := "id"
		if name == "stance_history" {
			key = "topic"
		}
		if status, _ := w.post(t, "/v1/"+name, withSession, string(mustJSON(t, map[string]any{key: "missing-private-id"}))); status != http.StatusNotFound {
			t.Errorf("%s missing handle = %d", name, status)
		}
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
	if trace.Source != source || trace.Artifact != session || len(trace.Events) != 13 {
		t.Fatalf("trace = %+v", trace)
	}
	for i, kind := range []connector.Kind{connector.KindAgentSession, connector.KindAgentTurn, connector.KindToolCall, connector.KindAudit, connector.KindAudit, connector.KindAudit, connector.KindAudit, connector.KindAudit, connector.KindAudit, connector.KindAudit, connector.KindAudit, connector.KindAudit, connector.KindAudit} {
		if trace.Events[i].Kind != kind {
			t.Errorf("event %d kind = %s, want %s", i, trace.Events[i].Kind, kind)
		}
	}
	var records []api.AuditRecord
	for _, ev := range trace.Events[3:] {
		var audit api.AuditRecord
		if err := json.Unmarshal(ev.Payload.Native, &audit); err != nil || audit.Session != session || audit.SessionSource != source {
			t.Errorf("audit = %+v, %v", audit, err)
		}
		if bytes.Contains(ev.Payload.Native, []byte("lock")) || bytes.Contains(ev.Payload.Native, []byte(`"query"`)) || bytes.Contains(ev.Payload.Native, []byte(`"text"`)) {
			t.Errorf("audit contains query text: %s", ev.Payload.Native)
		}
		records = append(records, audit)
	}
	for i, call := range []string{"get_bundle", "get_bundle", "get_l1", "stance_history", "search", "resolve", "get_l0", "get_l1", "get_l0", "stance_history"} {
		if records[i].Call != call {
			t.Errorf("audit %d call = %q, want %q", i, records[i].Call, call)
		}
	}
	sections := records[0].Sections
	if len(sections) != 6 || len(sections["anchors"].L1) == 0 || sections["anchors"].L1[0] != served.Anchors[0].L1 ||
		len(sections["stances"].Topics) == 0 || sections["stances"].Topics[0] != served.Stances[0].TopicID {
		t.Errorf("bundle section ids = %+v", sections)
	}
	for i, e := range served.Scope.Entities {
		if sections["scope.entities"].Entities[i] != e.ID {
			t.Errorf("scope entity %d = %q, want %q", i, sections["scope.entities"].Entities[i], e.ID)
		}
	}
	for i, item := range served.Recent.Items {
		if sections["recent"].L1[i] != item.L1 {
			t.Errorf("recent id %d = %q, want %q", i, sections["recent"].L1[i], item.L1)
		}
	}
	for i, q := range served.OpenQuestions {
		if sections["open_questions"].L1[i] != q.Evidence[0] {
			t.Errorf("question evidence %d = %q, want %q", i, sections["open_questions"].L1[i], q.Evidence[0])
		}
	}
	if len(records[2].TargetIDs) != 1 || records[2].TargetIDs[0] != served.Anchors[0].L1 ||
		len(records[3].TargetIDs) != 1 || records[3].TargetIDs[0] != served.Stances[0].TopicID || len(records[4].ReturnedIDs) == 0 {
		t.Errorf("successful handle audits = %+v", records)
	}
	for _, rec := range records[7:] {
		if rec.Status != http.StatusNotFound || len(rec.TargetIDs) != 0 || len(rec.ReturnedIDs) != 0 {
			t.Errorf("refused handle audit reveals ids: %+v", rec)
		}
	}
	before := len(records)
	var unlinkedBefore int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l0_events WHERE source = $1 AND kind = $2 AND payload->'native'->>'call' IN ('get_l1', 'stance_history', 'search') AND payload->'native'->>'session' IS NULL AND (payload->'native'->'target_ids' ? $3 OR payload->'native'->>'scope' = $4)`, api.AuditSource, string(connector.KindAudit), served.Anchors[0].L1, w.scope).Scan(&unlinkedBefore); err != nil {
		t.Fatal(err)
	}
	w.http(t, shedKyle, "get_l1", map[string]any{"id": served.Anchors[0].L1})
	w.http(t, shedKyle, "stance_history", map[string]any{"topic": served.Stances[0].TopicID})
	w.http(t, shedKyle, "search", map[string]any{"query": "lock", "scope": w.scope})
	var unlinkedAfter int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l0_events WHERE source = $1 AND kind = $2 AND payload->'native'->>'call' IN ('get_l1', 'stance_history', 'search') AND payload->'native'->>'session' IS NULL AND (payload->'native'->'target_ids' ? $3 OR payload->'native'->>'scope' = $4)`, api.AuditSource, string(connector.KindAudit), served.Anchors[0].L1, w.scope).Scan(&unlinkedAfter); err != nil {
		t.Fatal(err)
	}
	if unlinkedAfter != unlinkedBefore {
		t.Errorf("unlinked handles wrote %d audits", unlinkedAfter-unlinkedBefore)
	}
	var after api.SessionTrace
	if err := json.Unmarshal(w.http(t, kyle, "get_session", args), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != before+3 {
		t.Errorf("session grew after unlinked handles: %d events", len(after.Events))
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
