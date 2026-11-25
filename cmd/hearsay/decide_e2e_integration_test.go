//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/api/apitest"
)

func (r *e2eRun) callAs(who, agent string, mcp bool, call string, args any) (int, []byte) {
	r.t.Helper()
	body, _ := json.Marshal(args)
	path := "/v1/" + call
	if mcp {
		path = "/mcp"
		body = apitest.ToolCall(r.t, call, json.RawMessage(body))
	}
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, "http://"+r.apiAddr+path, bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.PrincipalHeader, who)
	req.Header.Set(api.AuthorizationHeader, "Bearer "+r.tokens[who])
	if agent != "" {
		req.Header.Set(api.AgentHeader, agent)
		req.Header.Set(api.AgentTokenHeader, r.tokens[agent])
	}
	code, got := r.do(req)
	if !mcp || code != http.StatusOK {
		return code, []byte(got)
	}
	text, isError, ok := apitest.ToolText([]byte(got))
	if !ok {
		r.t.Fatalf("POST /mcp %s answered %s", call, got)
	}
	if isError {
		return -1, []byte(text)
	}
	return http.StatusOK, []byte(text)
}

func TestDecideEndToEnd(t *testing.T) {
	stub := &modelStub{}
	stubServer := httptest.NewServer(stub)
	t.Cleanup(stubServer.Close)

	config := e2eConfig(t, stubServer.URL)
	const annToken = "    token_env: HEARSAY_E2E_ANN_TOKEN\n"
	if !strings.Contains(config, annToken) {
		t.Fatalf("the e2e configuration does not list ann:\n%s", config)
	}
	config = strings.Replace(config, annToken, annToken+"  - id: kern\n    kind: agent\n    class: worker\n    token_env: HEARSAY_E2E_KERN_TOKEN\n    scopes: [\"*\"]\n", 1)
	dir := writeStackConfig(t, config)
	tokens := map[string]string{"osmia": "e2e-osmia-token", "kyle": "e2e-kyle-token", "ann": "e2e-ann-token", "kern": "e2e-kern-token"}
	database := dbtest.Create(t, "e2e")
	t.Setenv("HEARSAY_DATABASE_URL", database)
	t.Setenv("HEARSAY_OSMIA_TOKEN", tokens["osmia"])
	t.Setenv("HEARSAY_E2E_KYLE_TOKEN", tokens["kyle"])
	t.Setenv("HEARSAY_E2E_ANN_TOKEN", tokens["ann"])
	t.Setenv("HEARSAY_E2E_KERN_TOKEN", tokens["kern"])
	t.Setenv("HEARSAY_E2E_MODEL_KEY", "model-stub-key")
	t.Setenv("HEARSAY_DISTILL_QUIET", "0")

	r := newE2ERun(t, tokens)
	r.startAll(dir, "/healthz")
	t.Logf("EVIDENCE: `hearsay all` (connectors %s, API %s), run in-process by cmd/hearsay's run() against Postgres, scratch database; model provider replaced by a stub at %s", r.hooks, r.apiAddr, stubServer.URL)

	const proj = "work:projects:payments"
	ts := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	r.must("projects", map[string]any{"kind": "project", "project": "payments", "name": "Payments rewrite", "description": "Move settlement off the legacy queue.", "author": "kyle", "created_at": ts, "updated_at": ts})
	r.must("projects", map[string]any{"kind": "decision", "project": "payments", "decision": "relay-1", "outcome": "Relayed: the owner wants jitter on every retry.", "decider": "chief_of_staff", "created_at": ts, "updated_at": ts})

	var topicID string
	waitUntil(t, "relay-1 being served", 3*time.Minute, r.stderr, func() string {
		b, _ := r.bundle("kyle", proj)
		for _, s := range b.Stances {
			if s.Decision != nil && s.Decision.ID == "relay-1" {
				topicID = s.TopicID
				return ""
			}
		}
		return "relay-1 is not in the bundle yet"
	})

	history := func() api.History {
		t.Helper()
		code, raw := r.read("kyle", "stance_history", map[string]string{"topic": topicID})
		if code != http.StatusOK {
			t.Fatalf("stance_history = %d %s", code, raw)
		}
		var h api.History
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	stanceID := history().Current
	served := func(step string) string {
		t.Helper()
		h := history()
		b, raw := r.bundle("kyle", proj)
		r.keep("get_bundle-"+step, raw)
		bundleTier := ""
		for _, s := range b.Stances {
			if s.TopicID == topicID {
				bundleTier = s.Tier
			}
		}
		g, raw, code := r.traverse("kyle", proj, "topics")
		if code != http.StatusOK {
			t.Fatalf("traverse = %d %s", code, raw)
		}
		r.keep("traverse-"+step, raw)
		traverseTier := ""
		for _, n := range g.Nodes {
			if n.Type == "topic" && n.ID == topicID {
				traverseTier = n.Tier
			}
		}
		if h.Tier != bundleTier || h.Tier != traverseTier {
			t.Errorf("%s: stance_history serves %q, get_bundle %q, traverse %q; want one tier", step, h.Tier, bundleTier, traverseTier)
		}
		t.Logf("EVIDENCE: after %s the topic stands at tier %q on stance_history, get_bundle and traverse", step, h.Tier)
		return h.Tier
	}
	if got := served("start"); got != "inferred" {
		t.Fatalf("the relayed decision stands at %q before any decision, want inferred", got)
	}
	stubBefore := fmt.Sprint(stub.schemasCopy())

	decide := func(who, agent string, mcp bool, args map[string]any) (int, api.Decided, []byte) {
		t.Helper()
		code, raw := r.callAs(who, agent, mcp, "decide", args)
		var out api.Decided
		if code == http.StatusOK {
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decide answered %s", raw)
			}
		}
		via := map[bool]string{false: "HTTP", true: "MCP"}[mcp]
		t.Logf("EVIDENCE: decide over %s as %s carried by %q %v -> %s", via, who, agent, args, raw)
		return code, out, raw
	}

	code, ratify, _ := decide("kyle", "", false, map[string]any{"decision_id": "e2e-ratify", "action": "ratify", "stance_id": stanceID})
	if code != http.StatusOK || !ratify.Recorded || ratify.StanceID != stanceID || ratify.GestureID == "" {
		t.Fatalf("the ratify over HTTP = %d %+v", code, ratify)
	}
	if got := served("ratify-over-http"); got != "ratified" {
		t.Errorf("after the ratify the topic stands at %q, want ratified", got)
	}
	code, demote, demoteBytes := decide("kyle", "kern", true, map[string]any{"decision_id": "e2e-demote", "action": "demote", "stance_id": stanceID})
	if code != http.StatusOK || !demote.Recorded {
		t.Fatalf("the demote over MCP = %d %+v", code, demote)
	}
	if got := served("demote-over-mcp"); got != "contested" {
		t.Errorf("after the demote the topic stands at %q, want contested", got)
	}
	_, over, overBytes := decide("kyle", "kern", false, map[string]any{"decision_id": "e2e-demote", "action": "demote", "stance_id": stanceID})
	_, via, viaBytes := decide("kyle", "kern", true, map[string]any{"decision_id": "e2e-demote", "action": "demote", "stance_id": stanceID})
	if over.Recorded || via.Recorded || over.GestureID != demote.GestureID || string(overBytes) != string(viaBytes) {
		t.Errorf("replays = %s and %s, want the same bytes naming gesture %s, not recorded (first answer %s)", overBytes, viaBytes, demote.GestureID, demoteBytes)
	}
	code, undoDemote, _ := decide("kyle", "kern", true, map[string]any{"decision_id": "e2e-undo-demote", "action": "undo", "gesture_id": demote.GestureID})
	if code != http.StatusOK || !undoDemote.Recorded || undoDemote.UndoneGestureID != demote.GestureID {
		t.Fatalf("the undo of the demote = %d %+v", code, undoDemote)
	}
	if got := served("undo-demote-over-mcp"); got != "ratified" {
		t.Errorf("after the undo of the demote the topic stands at %q, want ratified", got)
	}
	if code, _, raw := decide("kyle", "", false, map[string]any{"decision_id": "e2e-undo-ratify", "action": "undo", "gesture_id": ratify.GestureID}); code != http.StatusOK {
		t.Fatalf("the undo of the ratify = %d %s", code, raw)
	}
	if got := served("undo-ratify-over-http"); got != "inferred" {
		t.Errorf("after both undos the topic stands at %q, want inferred", got)
	}

	for _, tt := range []struct {
		name, agent, code string
		args              map[string]any
		status            int
	}{
		{"an observer carrying", "osmia", "not_allowed", map[string]any{"decision_id": "e2e-observer", "action": "ratify", "stance_id": stanceID}, http.StatusForbidden},
		{"a stance that does not exist", "", "not_found", map[string]any{"decision_id": "e2e-none", "action": "ratify", "stance_id": "stance:none"}, http.StatusNotFound},
		{"a pin", "", "invalid", map[string]any{"decision_id": "e2e-pin", "action": "pin", "stance_id": stanceID}, http.StatusBadRequest},
		{"an undone gesture", "", "not_undoable", map[string]any{"decision_id": "e2e-again", "action": "undo", "gesture_id": ratify.GestureID}, http.StatusConflict},
		{"another stance under a used id", "", "conflict", map[string]any{"decision_id": "e2e-ratify", "action": "demote", "stance_id": stanceID}, http.StatusConflict},
	} {
		status, raw := r.callAs("kyle", tt.agent, false, "decide", tt.args)
		var refusal struct{ Error, Code string }
		if json.Unmarshal(raw, &refusal) != nil || refusal.Code != tt.code || refusal.Error == "" || status != tt.status {
			t.Errorf("%s = %d %s, want %d with code %s and a message", tt.name, status, raw, tt.status, tt.code)
		}
		t.Logf("EVIDENCE: decide %s -> %d %s", tt.name, status, raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+r.apiAddr+"/v1/decide", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.PrincipalHeader, "kyle")
	req.Header.Set(api.AuthorizationHeader, "Bearer not-the-credential")
	if status, raw := r.do(req); status != http.StatusUnauthorized || strings.Contains(raw, `"code"`) {
		t.Errorf("decide with a bad credential = %d %s, want 401 with no code", status, raw)
	}
	if got := served("refusals"); got != "inferred" {
		t.Errorf("after the refusals the topic stands at %q, want inferred", got)
	}

	list := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if err := run(t.Context(), append([]string{"gestures", "list", "--config", dir, "--principal", "kyle"}, args...), &out, &errOut); err != nil {
			t.Fatalf("hearsay gestures list %v = %v\n%s", args, err, errOut.String())
		}
		return out.String()
	}
	table := list("--scope", "work")
	t.Logf("EVIDENCE: hearsay gestures list\n%s", table)
	raw := list("--json")
	if r.evidence != "" {
		_ = os.WriteFile(filepath.Join(r.evidence, "gestures-list.json"), []byte(raw), 0o644)
	}
	var records []struct {
		ID        int64    `json:"id"`
		Action    string   `json:"action"`
		Principal string   `json:"principal"`
		Carrier   string   `json:"carrier"`
		Stances   []string `json:"stances"`
		Undoes    int64    `json:"undoes"`
	}
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&records); err != nil {
		t.Fatalf("gestures list --json = %s: %v", raw, err)
	}
	want := map[string][2]string{
		ratify.GestureID:     {"ratify", ""},
		demote.GestureID:     {"demote", "kern"},
		undoDemote.GestureID: {"undo", "kern"},
	}
	seen := 0
	for _, g := range records {
		id := fmt.Sprint(g.ID)
		if w, ok := want[id]; ok {
			seen++
			if g.Action != w[0] || g.Principal != "kyle" || g.Carrier != w[1] {
				t.Errorf("gesture %s lists as %s by %q carried by %q, want %s by kyle carried by %q", id, g.Action, g.Principal, g.Carrier, w[0], w[1])
			}
		}
	}
	if seen != len(want) {
		t.Errorf("gestures list --json shows %d of the %d decisions: %s", seen, len(want), raw)
	}
	if !strings.Contains(table, "CARRIER") || !strings.Contains(table, "kern") {
		t.Errorf("the gestures table has no carrier column naming kern:\n%s", table)
	}

	if after := fmt.Sprint(stub.schemasCopy()); after != stubBefore {
		t.Errorf("the model stub saw calls during the decisions: before %s, after %s", stubBefore, after)
	}
	if t.Failed() {
		t.Logf("hearsay all log:\n%s", r.stderr.String())
	}
}

func (m *modelStub) schemasCopy() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{"prompts": len(m.prompts)}
	for k, v := range m.schemas {
		out[k] = v
	}
	return out
}
