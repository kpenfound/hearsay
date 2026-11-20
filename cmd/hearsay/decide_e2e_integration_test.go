//go:build integration

package main

// The owner-decision call, end to end, in a running Hearsay.
//
// Like TestWorkRecordsEndToEnd, this runs the shipped command, `hearsay all`,
// against a database of its own in the Postgres HEARSAY_DATABASE_URL names, with
// the documented owner/work configuration plus one worker-class agent, `kern`,
// that carries a decision. It posts a work decision to the connectors' real
// HTTP listener, waits until the distiller and the assertion worker serve it,
// and then, as people and agents do through the API's real HTTP listener:
//
//   - ratifies its stance as the owner over HTTP, with no agent;
//   - demotes it as the owner over MCP, with kern carrying the decision;
//   - undoes the demote over MCP (the ratify stands again) and the ratify over
//     HTTP (the stance is inferred again);
//   - replays a decision over both interfaces, and has an observer refused;
//
// and after each step reads the topic's standing through stance_history,
// get_bundle and traverse, and finally the ledger through `hearsay gestures
// list`, in its table and in --json, to see each decision with its principal
// and its carrier. Nothing but the model provider is replaced, by a local stub
// that must see no call from the decide path.
//
//	HEARSAY_DATABASE_URL=postgres://... go test -tags=integration -count=1 -v \
//	    -run TestDecideEndToEnd ./cmd/hearsay
//
// `deploy/smoke/decide-e2e.sh` runs it in Docker alone. Set HEARSAY_E2E_EVIDENCE
// to a directory to write each read's raw output there.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/service/api"
)

// post sends a request as a person, with an agent carrying it when one is
// named, over HTTP (`/v1/<call>`) or MCP (`/mcp`), and returns the status and
// the bytes served: the response body, or the tool result's text content.
func (r *e2eRun) callAs(who, agent string, mcp bool, call string, args any) (int, []byte) {
	r.t.Helper()
	body, _ := json.Marshal(args)
	path := "/v1/" + call
	if mcp {
		path = "/mcp"
		body, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": call, "arguments": json.RawMessage(body)}})
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
	var rpc struct {
		Result api.ToolResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &rpc); err != nil || len(rpc.Result.Content) != 1 {
		r.t.Fatalf("POST /mcp %s answered %s", call, got)
	}
	if rpc.Result.IsError {
		return -1, []byte(rpc.Result.Content[0].Text)
	}
	return http.StatusOK, []byte(rpc.Result.Content[0].Text)
}

func TestDecideEndToEnd(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	stub := &modelStub{}
	stubServer := httptest.NewServer(stub)
	t.Cleanup(stubServer.Close)

	config := e2eConfig(t, stubServer.URL)
	const annToken = "    token_env: HEARSAY_E2E_ANN_TOKEN\n"
	if !strings.Contains(config, annToken) {
		t.Fatalf("the e2e configuration no longer lists ann:\n%s", config)
	}
	config = strings.Replace(config, annToken, annToken+"  - id: kern\n    kind: agent\n    class: worker\n    token_env: HEARSAY_E2E_KERN_TOKEN\n    scopes: [\"*\"]\n", 1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hearsay.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{"osmia": "e2e-osmia-token", "kyle": "e2e-kyle-token", "ann": "e2e-ann-token", "kern": "e2e-kern-token"}
	database := scratchDatabase(t, url)
	t.Setenv("HEARSAY_DATABASE_URL", database)
	t.Setenv("HEARSAY_OSMIA_TOKEN", tokens["osmia"])
	t.Setenv("HEARSAY_E2E_KYLE_TOKEN", tokens["kyle"])
	t.Setenv("HEARSAY_E2E_ANN_TOKEN", tokens["ann"])
	t.Setenv("HEARSAY_E2E_KERN_TOKEN", tokens["kern"])
	t.Setenv("HEARSAY_E2E_MODEL_KEY", "model-stub-key")
	t.Setenv("HEARSAY_DISTILL_QUIET", "0")

	r := &e2eRun{t: t, hooks: freeAddr(t), apiAddr: freeAddr(t), tokens: tokens, stderr: &syncBuffer{}, evidence: os.Getenv("HEARSAY_E2E_EVIDENCE")}
	if r.evidence != "" {
		if err := os.MkdirAll(r.evidence, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	distillerAddr, assertAddr := freeAddr(t), freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"all", "--config", dir, "--listen", r.hooks, "--api-listen", r.apiAddr,
			"--distiller-listen", distillerAddr, "--assert-worker-listen", assertAddr, "--log-format", "json"}, io.Discard, r.stderr)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("hearsay all stopped with %v\n%s", err, r.stderr.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("hearsay all did not stop")
		}
	})
	t.Logf("EVIDENCE: `hearsay all` (connectors %s, API %s), run in-process by cmd/hearsay's run() against Postgres, scratch database; model provider replaced by a stub at %s", r.hooks, r.apiAddr, stubServer.URL)
	for _, addr := range []string{r.hooks, r.apiAddr, distillerAddr, assertAddr} {
		deadline := time.Now().Add(60 * time.Second)
		for {
			resp, err := http.Get("http://" + addr + "/healthz")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never answered /healthz:\n%s", addr, r.stderr.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// A work decision becomes a topic with one inferred stance.
	const proj = "work:projects:payments"
	ts := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	r.must("projects", map[string]any{"kind": "project", "project": "payments", "name": "Payments rewrite", "description": "Move settlement off the legacy queue.", "author": "kyle", "created_at": ts, "updated_at": ts})
	r.must("projects", map[string]any{"kind": "decision", "project": "payments", "decision": "relay-1", "outcome": "Relayed: the owner wants jitter on every retry.", "decider": "chief_of_staff", "created_at": ts, "updated_at": ts})

	var topicID string
	deadline := time.Now().Add(3 * time.Minute)
	for topicID == "" {
		b, _ := r.bundle("kyle", proj)
		for _, s := range b.Stances {
			if s.Decision != nil && s.Decision.ID == "relay-1" {
				topicID = s.TopicID
			}
		}
		if topicID == "" && time.Now().After(deadline) {
			t.Fatalf("relay-1 was not served within 3 minutes:\n%s", r.stderr.String())
		}
		if topicID == "" {
			time.Sleep(200 * time.Millisecond)
		}
	}

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
	// served is the tier all three reads serve the topic at, failing the test
	// where they disagree.
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

	// The owner ratifies over HTTP, calling for themselves.
	code, ratify, _ := decide("kyle", "", false, map[string]any{"decision_id": "e2e-ratify", "action": "ratify", "stance_id": stanceID})
	if code != http.StatusOK || !ratify.Recorded || ratify.StanceID != stanceID || ratify.GestureID == "" {
		t.Fatalf("the ratify over HTTP = %d %+v", code, ratify)
	}
	if got := served("ratify-over-http"); got != "ratified" {
		t.Errorf("after the ratify the topic stands at %q, want ratified", got)
	}
	// ...and demotes over MCP with kern carrying the decision.
	code, demote, demoteBytes := decide("kyle", "kern", true, map[string]any{"decision_id": "e2e-demote", "action": "demote", "stance_id": stanceID})
	if code != http.StatusOK || !demote.Recorded {
		t.Fatalf("the demote over MCP = %d %+v", code, demote)
	}
	if got := served("demote-over-mcp"); got != "contested" {
		t.Errorf("after the demote the topic stands at %q, want contested", got)
	}
	// A replay over each interface answers the original, and records nothing.
	_, over, overBytes := decide("kyle", "kern", false, map[string]any{"decision_id": "e2e-demote", "action": "demote", "stance_id": stanceID})
	_, via, viaBytes := decide("kyle", "kern", true, map[string]any{"decision_id": "e2e-demote", "action": "demote", "stance_id": stanceID})
	if over.Recorded || via.Recorded || over.GestureID != demote.GestureID || string(overBytes) != string(viaBytes) {
		t.Errorf("replays = %s and %s, want the same bytes naming gesture %s, not recorded (first answer %s)", overBytes, viaBytes, demote.GestureID, demoteBytes)
	}
	// An undo of the demote over MCP puts the ratify back.
	code, undoDemote, _ := decide("kyle", "kern", true, map[string]any{"decision_id": "e2e-undo-demote", "action": "undo", "gesture_id": demote.GestureID})
	if code != http.StatusOK || !undoDemote.Recorded || undoDemote.UndoneGestureID != demote.GestureID {
		t.Fatalf("the undo of the demote = %d %+v", code, undoDemote)
	}
	if got := served("undo-demote-over-mcp"); got != "ratified" {
		t.Errorf("after the undo of the demote the topic stands at %q, want ratified", got)
	}
	// An undo of the ratify over HTTP.
	if code, _, raw := decide("kyle", "", false, map[string]any{"decision_id": "e2e-undo-ratify", "action": "undo", "gesture_id": ratify.GestureID}); code != http.StatusOK {
		t.Fatalf("the undo of the ratify = %d %s", code, raw)
	}
	if got := served("undo-ratify-over-http"); got != "inferred" {
		t.Errorf("after both undos the topic stands at %q, want inferred", got)
	}

	// Refusals carry a code; the observer may not carry, and nothing is recorded.
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
	// A bad credential is a shared failure: its status, no code.
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

	// The ledger, as `hearsay gestures list` shows it.
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
	want := map[string][2]string{ // gesture id -> action, carrier
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

	// The decide path made no model call and no judgment.
	if after := fmt.Sprint(stub.schemasCopy()); after != stubBefore {
		t.Errorf("the model stub saw calls during the decisions: before %s, after %s", stubBefore, after)
	}
	if t.Failed() {
		t.Logf("hearsay all log:\n%s", r.stderr.String())
	}
}

// schemasCopy is what the stub has been asked, by schema.
func (m *modelStub) schemasCopy() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{"prompts": len(m.prompts)}
	for k, v := range m.schemas {
		out[k] = v
	}
	return out
}
