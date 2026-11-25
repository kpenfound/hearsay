//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/service/api"
)

const (
	memoryScope = "code:kern-memory"
	sharedTopic = "Who owns the shared cache?"
	deletedWord = "ORCHID"
	summaryWord = "crew"
)

type conversationStub struct {
	mu    sync.Mutex
	calls map[string]int
}

func (s *conversationStub) count(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[name]++
}

func (s *conversationStub) counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.calls {
		out[k] = v
	}
	return out
}

func (s *conversationStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/systemone" {
		s.judge(w, r)
		return
	}
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if r.URL.Path != "/v1/messages" || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Tools) != 1 || len(req.Messages) == 0 {
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"the stub answers structured Messages calls only"}}`, http.StatusBadRequest)
		return
	}
	tool, text := req.Tools[0].Name, req.Messages[0].Content
	s.count(tool)
	var answer any
	switch tool {
	case "conversation_topics":
		answer = stubSegments(text)
	case "l1_conversation_segment":
		answer = stubSegmentBody(text)
	case "l1_issue":
		answer = map[string]any{
			"summary":  "Proposal about shared cache ownership: each service should own its cache lines.",
			"question": sharedTopic,
			"outcome":  "Each service owns its own shared cache lines.",
		}
	case "l2_assertions":
		position := "Each service owns its own shared cache lines."
		if _, after, ok := strings.Cut(text, "\n\n"); ok && strings.Contains(after, deletedWord) {
			position = strings.TrimSpace(after)
		}
		answer = map[string]any{"assertions": []any{map[string]any{"topic_name": sharedTopic, "position": position}}}
	default:
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"the stub does not know this schema"}}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_stub", "type": "message", "role": "assistant", "model": "stub", "stop_reason": "tool_use",
		"content": []any{map[string]any{"type": "tool_use", "id": "toolu_stub", "name": tool, "input": answer}},
		"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
	})
}

func stubTopicOf(line string) string {
	switch lower := strings.ToLower(line); {
	case strings.Contains(lower, "cache"):
		return "shared cache ownership"
	case strings.Contains(lower, "lisbon"):
		return "lisbon trip"
	}
	return ""
}

func stubSegments(text string) any {
	segments := []any{}
	var topic string
	start := 0
	flush := func(end int) {
		if topic != "" {
			segments = append(segments, map[string]any{"topic": topic, "start": start, "end": end})
		}
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		now := stubTopicOf(line)
		if now != topic {
			flush(i)
			topic, start = now, i+1
		}
	}
	flush(len(lines))
	return map[string]any{"segments": segments}
}

func stubSegmentBody(text string) any {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, deletedWord) {
			continue
		}
		number, _, _ := strings.Cut(line, ".")
		n, err := strconv.Atoi(number)
		if err != nil {
			break
		}
		return map[string]any{
			"summary":       "Shared cache ownership settled: the platform " + summaryWord + " looks after the cache.",
			"question":      sharedTopic,
			"outcome":       "The platform team owns the shared cache, codename " + deletedWord + ".",
			"outcome_lines": []int{n},
		}
	}
	if strings.Contains(strings.ToLower(text), "lisbon") {
		return map[string]any{"summary": "Planning the March flights to Lisbon; the earlier morning flight was booked."}
	}
	return map[string]any{"summary": "The shared cache came up."}
}

func (s *conversationStub) judge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Questions) == 0 {
		http.Error(w, `{"error":"the stub answers System One calls only"}`, http.StatusBadRequest)
		return
	}
	s.count("judgment")
	owner, _, _ := strings.Cut(req.State, "Context from Kern:")
	answers := map[string]any{}
	for id := range req.Questions {
		yes := false
		switch {
		case id == "binding_agreement":
			yes = strings.HasPrefix(req.State, "Owner's words:") && strings.Contains(owner, deletedWord)
		case id == "unagreed_proposal":
			yes = strings.Contains(req.State, "Nobody has agreed")
		case strings.HasPrefix(id, "T"):
			yes = true
		}
		p := 0.02
		if yes {
			p = 0.98
		}
		answers[id] = map[string]any{"type": "noul", "noul": p}
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-stub", "answers": answers, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}})
}

func conversationConfig(t *testing.T, stubURL string) string {
	t.Helper()
	text := conversationExample(t)
	replace := func(old, new string) {
		t.Helper()
		if !strings.Contains(text, old) {
			t.Fatalf("the documented conversation example does not have %q:\n%s", old, text)
		}
		text = strings.Replace(text, old, new, 1)
	}
	replace("sources:\n  - id: kern\n", `sources:
  - id: tracker
    type: tracker
    containers: [ENG]
    settings:
      access:
        ENG: [{kind: public}]
    secrets:
      token: HEARSAY_E2E_TRACKER_TOKEN
  - id: kern
`)
	replace("  - id: kyle\n    kind: human\n", "  - id: kyle\n    kind: human\n    token_env: HEARSAY_E2E_KYLE_TOKEN\n")
	replace("\nscopes:\n  - id: memory\n    sources: [kern]\n", `  - id: ann
    kind: human
    token_env: HEARSAY_E2E_ANN_TOKEN
    identities:
      - source: tracker
        native_id: u-ann
  - id: scout
    kind: agent
    class: orchestrator
    token_env: HEARSAY_E2E_SCOUT_TOKEN
    scopes: ["*"]
    identities:
      - source: tracker
        native_id: scout

scopes:
  - id: memory
    sources: [kern, tracker]
    entities: [`+memoryScope+`]

code:
  - id: `+memoryScope+`
    type: project
    name: Kern memory

authority:
  - scope: "*"
    ranking: [dm, merged_pr, issue]
`)
	return text + e2eLLMSection(stubURL)
}

func TestConversationMemoryEndToEnd(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	stub := &conversationStub{}
	stubServer := httptest.NewServer(stub)
	t.Cleanup(stubServer.Close)

	dir := writeStackConfig(t, conversationConfig(t, stubServer.URL))
	tokens := map[string]string{"kyle": "e2e-kyle-token", "ann": "e2e-ann-token", "kern": "e2e-kern-token", "scout": "e2e-scout-token", "tracker": "e2e-tracker-token"}
	database := createDatabase(t, url, "e2e_work_")
	t.Setenv("HEARSAY_DATABASE_URL", database)
	t.Setenv("HEARSAY_E2E_KYLE_TOKEN", tokens["kyle"])
	t.Setenv("HEARSAY_E2E_ANN_TOKEN", tokens["ann"])
	t.Setenv("HEARSAY_KERN_TOKEN", tokens["kern"])
	t.Setenv("HEARSAY_E2E_SCOUT_TOKEN", tokens["scout"])
	t.Setenv("HEARSAY_E2E_TRACKER_TOKEN", tokens["tracker"])
	t.Setenv("HEARSAY_E2E_MODEL_KEY", "model-stub-key")
	t.Setenv("HEARSAY_DISTILL_QUIET", "3s")

	var validated, validatedErr bytes.Buffer
	if err := run(t.Context(), []string{"config", "validate", dir}, &validated, &validatedErr); err != nil {
		t.Fatalf("hearsay config validate = %v\n%s", err, validatedErr.String())
	}
	t.Logf("EVIDENCE: hearsay config validate accepts the documented conversation source:\n%s", validated.String())

	r := newE2ERun(t, tokens)
	stop, distillerAddr, assertAddr := r.startAll(dir, "/healthz", "/readyz")
	t.Logf("EVIDENCE: `hearsay all` (connectors %s, API %s, distiller %s, assertion worker %s), run in-process by cmd/hearsay's run() against Postgres, scratch database; model provider and Jev replaced by a local stub at %s", r.hooks, r.apiAddr, distillerAddr, assertAddr, stubServer.URL)

	as := func(who, agent, call string, args any) (int, []byte) {
		t.Helper()
		return r.callAs(who, agent, false, call, args)
	}
	mustAs := func(who, agent, call string, args any, into any) []byte {
		t.Helper()
		code, raw := as(who, agent, call, args)
		if code != http.StatusOK {
			t.Fatalf("%s as %s (agent %q) = %d %s\n%s", call, who, agent, code, raw, r.stderr.String())
		}
		if into != nil {
			if err := json.Unmarshal(raw, into); err != nil {
				t.Fatalf("%s answered %s: %v", call, raw, err)
			}
		}
		return raw
	}
	eventually := func(what string, within time.Duration, check func() string) {
		t.Helper()
		waitUntil(t, what, within, r.stderr, check)
	}
	postTo := func(source, token string, body map[string]any) (int, string) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+r.hooks+"/hooks/"+source, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		return r.do(req)
	}
	type stored struct {
		EventID string `json:"event_id"`
		Status  string `json:"status"`
	}
	conversationBase := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	message := func(exchange, role, text string, minutes int, interrupted bool) map[string]any {
		m := map[string]any{"owner": "kyle", "conversation": "conv-1", "exchange": exchange, "role": role,
			"time": conversationBase.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339), "text": text}
		if interrupted {
			m["interrupted"] = true
		}
		return m
	}
	hook := func(body map[string]any) (int, string) {
		t.Helper()
		return postTo("kern", tokens["kern"], body)
	}
	eventCount := func(source string) int {
		t.Helper()
		conn, err := pgx.Connect(t.Context(), database)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
		var n int
		if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM l0_events WHERE source = $1`, source).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	bundleOf := func(who, agent string) (bundle.Bundle, []byte) {
		t.Helper()
		var b bundle.Bundle
		raw := mustAs(who, agent, "get_bundle", map[string]string{"scope": memoryScope}, &b)
		return b, raw
	}
	coverage := func(who, agent string, ids []string) (api.Covered, []byte) {
		t.Helper()
		var c api.Covered
		raw := mustAs(who, agent, "coverage", map[string]any{"event_ids": ids}, &c)
		if len(c.Results) != len(ids) {
			t.Fatalf("coverage of %d ids answered %s", len(ids), raw)
		}
		return c, raw
	}
	search := func(who, agent string, args map[string]any) api.SearchResult {
		t.Helper()
		var found api.SearchResult
		mustAs(who, agent, "search", args, &found)
		return found
	}
	found := func(res api.SearchResult, id string) bool {
		for _, h := range res.Documents {
			if h.ID == id {
				return true
			}
		}
		return false
	}
	traverseRaw := func(who, agent string, steps ...string) (int, string) {
		t.Helper()
		var s []map[string]string
		for _, e := range steps {
			s = append(s, map[string]string{"edge": e})
		}
		code, raw := as(who, agent, "traverse", map[string]any{"start": []map[string]string{{"type": "entity", "id": memoryScope}}, "steps": s})
		return code, string(raw)
	}
	history := func(who, agent, topic string) (int, api.History, []byte) {
		t.Helper()
		code, raw := as(who, agent, "stance_history", map[string]string{"topic": topic})
		var h api.History
		if code == http.StatusOK {
			if err := json.Unmarshal(raw, &h); err != nil {
				t.Fatalf("stance_history answered %s", raw)
			}
		}
		return code, h, raw
	}
	type watched struct {
		code int
		out  api.Watched
		raw  []byte
	}
	watch := func(who, agent, after string) <-chan watched {
		ch := make(chan watched, 1)
		go func() {
			args := map[string]any{"scope": memoryScope}
			if after != "" {
				args["after"] = after
			}
			code, raw := r.callAs(who, agent, false, "watch", args)
			w := watched{code: code, raw: raw}
			_ = json.Unmarshal(raw, &w.out)
			ch <- w
		}()
		return ch
	}

	ownerWatch, annWatch := watch("kyle", "", ""), watch("ann", "", "")
	time.Sleep(time.Second)
	code, body := postTo("tracker", tokens["tracker"], map[string]any{
		"kind": "ticket", "project": "ENG", "id": "ENG-1", "title": sharedTopic,
		"body":   "Proposal: each service should own its own cache lines. Nobody has agreed to this yet.",
		"author": map[string]any{"id": "u-ann", "handle": "ann", "name": "Ann"}, "created_at": "2026-11-19T10:00:00Z", "updated_at": "2026-11-19T10:00:00Z",
	})
	if code != http.StatusAccepted {
		t.Fatalf("POST /hooks/tracker = %d %s", code, body)
	}
	var topicID, ticketDoc, ticketPosition string
	eventually("the ticket's stance opens the shared topic", 3*time.Minute, func() string {
		b, _ := bundleOf("ann", "")
		for _, s := range b.Stances {
			if s.Topic == sharedTopic && len(s.Evidence) == 1 {
				topicID, ticketDoc, ticketPosition = s.TopicID, s.Evidence[0], s.Current
				return ""
			}
		}
		return "ann's bundle serves " + fmt.Sprint(b.Stances)
	})
	t.Logf("EVIDENCE: ann reads the ticket's topic %s, its stance %q on document %s", topicID, ticketPosition, ticketDoc)
	var cursor string
	for who, ch := range map[string]<-chan watched{"kyle": ownerWatch, "ann": annWatch} {
		w := <-ch
		hasTicket := w.code == http.StatusOK && slices.ContainsFunc(w.out.Notifications, func(n api.Notification) bool { return n.Source == "tracker" })
		if !hasTicket {
			t.Errorf("%s's watch from before the ticket = %d %s, want the ticket's event", who, w.code, w.raw)
		}
		if who == "kyle" {
			cursor = w.out.Cursor
		}
	}
	if cursor == "" {
		t.Fatal("the owner's watch returned no cursor")
	}

	type say struct {
		exchange, role, text string
		minute               int
		interrupted          bool
	}
	script := []say{
		{"x0", "owner", "Good morning.", 0, false},
		{"x0", "assistant", "Good morning, Kyle.", 0, false},
		{"x1", "owner", "Please look at flights for the Lisbon trip in March.", 5, false},
		{"x1", "assistant", "I found two morning flights for the Lisbon trip.", 5, false},
		{"x2", "owner", "About the shared cache: I decided the platform team owns it, codename " + deletedWord + ".", 10, false},
		{"x2", "assistant", "Noted, the platform team owns the shared cache.", 10, false},
		{"x3", "owner", "Back to the Lisbon trip: book the earlier flight.", 15, false},
		{"x3", "assistant", "Booked the earlier Lisbon flight.", 15, false},
		{"x4", "owner", "Also the Lisbon hotel, near the river, and then I was cut off", 20, true},
	}
	ids := map[string]string{}
	var all []string
	for _, m := range script {
		code, raw := hook(message(m.exchange, m.role, m.text, m.minute, m.interrupted))
		var got stored
		if code != http.StatusAccepted || json.Unmarshal([]byte(raw), &got) != nil || got.EventID == "" || got.Status != "stored" {
			t.Fatalf("POST /hooks/kern %s:%s = %d %s, want 202 stored with an event id", m.exchange, m.role, code, raw)
		}
		ids[m.exchange+":"+m.role] = got.EventID
		all = append(all, got.EventID)
	}
	t.Logf("EVIDENCE: %d messages posted to /hooks/kern as the agent kern; events %v", len(all), all)
	exchange2 := []string{ids["x2:owner"], ids["x2:assistant"]}
	greeting := []string{ids["x0:owner"], ids["x0:assistant"]}
	lisbon := []string{ids["x1:owner"], ids["x1:assistant"], ids["x3:owner"], ids["x3:assistant"], ids["x4:owner"]}

	before := eventCount("kern")
	if code, raw := hook(map[string]any{"owner": "ann", "conversation": "conv-1", "exchange": "x9", "role": "owner", "time": "2026-11-20T10:00:00Z", "text": "not the owner's"}); code != http.StatusForbidden {
		t.Errorf("a post naming another owner = %d %s, want 403", code, raw)
	}
	if code, raw := postTo("kern", tokens["scout"], message("x9", "owner", "from an agent the source does not allow", 30, false)); code != http.StatusForbidden {
		t.Errorf("a post by an agent the source does not allow = %d %s, want 403", code, raw)
	}
	if code, raw := postTo("kern", "not-a-token", message("x9", "owner", "no credential", 30, false)); code != http.StatusUnauthorized {
		t.Errorf("a post with no credential = %d %s, want 401", code, raw)
	}
	if after := eventCount("kern"); after != before {
		t.Errorf("refused posts changed the conversation's events from %d to %d", before, after)
	}

	first, raw := coverage("kyle", "kern", all)
	r.keep("coverage-pending", raw)
	for _, res := range first.Results {
		if res.State != "pending" || len(res.Documents) != 0 {
			t.Errorf("coverage of %s right after the post = %s %v, want pending with no documents (the distiller has not read it)", res.EventID, res.State, res.Documents)
		}
	}
	var settled api.Covered
	eventually("every message leaves pending", 3*time.Minute, func() string {
		settled, _ = coverage("kyle", "kern", all)
		for _, res := range settled.Results {
			if res.State == "pending" {
				return res.EventID + " is still pending"
			}
		}
		return ""
	})
	_, raw = coverage("kyle", "kern", all)
	r.keep("coverage-covered", raw)
	state := map[string]api.CoverageResult{}
	for _, res := range settled.Results {
		state[res.EventID] = res
	}
	for _, id := range greeting {
		if state[id].State != "excluded" || len(state[id].Documents) != 0 {
			t.Errorf("the greeting %s = %+v, want excluded: no segment quotes it", id, state[id])
		}
	}
	var lisbonDoc, cacheDoc string
	for _, id := range lisbon {
		if state[id].State != "covered" || len(state[id].Documents) != 1 {
			t.Fatalf("the trip's message %s = %+v, want covered by one document", id, state[id])
		}
		if lisbonDoc == "" {
			lisbonDoc = state[id].Documents[0]
		}
		if state[id].Documents[0] != lisbonDoc {
			t.Errorf("the trip resumes as a second document: %s is cited by %v, %s by %s", id, state[id].Documents, lisbon[0], lisbonDoc)
		}
	}
	for _, id := range exchange2 {
		if state[id].State != "covered" || len(state[id].Documents) != 1 {
			t.Fatalf("the decision's message %s = %+v, want covered by one document", id, state[id])
		}
		cacheDoc = state[id].Documents[0]
	}
	if cacheDoc == lisbonDoc {
		t.Errorf("the decision and the trip are one document, %s", cacheDoc)
	}
	if code, over := r.callAs("kyle", "kern", true, "coverage", map[string]any{"event_ids": all}); code != http.StatusOK || string(over) != string(raw) {
		t.Errorf("coverage over MCP = %d %s, want the HTTP answer %s", code, over, raw)
	}
	t.Logf("EVIDENCE: coverage went pending -> covered; the trip (resumed) is one document %s, the decision %s, the greeting is excluded", lisbonDoc, cacheDoc)

	before = eventCount("kern")
	if code, raw := hook(message("x1", "owner", "Please look at flights for the Lisbon trip in March.", 5, false)); code != http.StatusAccepted || !strings.Contains(raw, ids["x1:owner"]) || !strings.Contains(raw, `"unchanged"`) {
		t.Errorf("an identical re-post = %d %s, want 202 unchanged naming %s", code, raw, ids["x1:owner"])
	}
	if after := eventCount("kern"); after != before {
		t.Errorf("an identical re-post wrote %d events", after-before)
	}

	window := map[string]any{"since": "2026-11-20T00:00:00Z", "until": "2026-11-21T00:00:00Z"}
	query := func(q string, extra map[string]any) map[string]any {
		args := map[string]any{"query": q}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	res := search("kyle", "kern", query(summaryWord, window))
	r.keep("search-summary-word", mustAs("kyle", "kern", "search", query(summaryWord, window), nil))
	if !found(res, cacheDoc) || res.Truncated {
		t.Errorf("search %q in the window = %+v, want the decision's document %s and truncated false", summaryWord, res, cacheDoc)
	}
	for _, c := range []struct {
		name  string
		extra map[string]any
	}{
		{"after the window", map[string]any{"since": "2026-11-21T00:00:00Z"}},
		{"before the window", map[string]any{"until": "2026-11-20T00:00:00Z"}},
	} {
		if got := search("kyle", "kern", query(summaryWord, c.extra)); len(got.Documents) != 0 {
			t.Errorf("search %q %s = %+v, want nothing", summaryWord, c.name, got)
		}
	}
	if code, raw := as("kyle", "kern", "search", query(summaryWord, map[string]any{"since": "2026-11-21T00:00:00Z", "until": "2026-11-20T00:00:00Z"})); code != http.StatusBadRequest || !strings.Contains(string(raw), "invalid_request") {
		t.Errorf("search with since after until = %d %s, want 400 invalid_request", code, raw)
	}
	if !found(search("kyle", "kern", query(deletedWord, window)), cacheDoc) {
		t.Errorf("search %q (a word of the messages themselves) did not find %s", deletedWord, cacheDoc)
	}

	var ownerStance api.StanceRecord
	eventually("the owner's decision becomes a stance on the shared topic", 3*time.Minute, func() string {
		code, h, raw := history("kyle", "kern", topicID)
		if code != http.StatusOK {
			return fmt.Sprintf("stance_history = %d %s", code, raw)
		}
		for _, st := range h.Stances {
			if strings.Contains(st.Position, deletedWord) {
				ownerStance = st
				return ""
			}
		}
		return "the history holds " + string(raw)
	})
	if !slices.Equal(ownerStance.Evidence, []string{cacheDoc}) || ownerStance.Author != "kyle" {
		t.Errorf("the owner's stance = %+v, want kyle's, with the decision's document %s as its evidence", ownerStance, cacheDoc)
	}
	_, h, raw := history("kyle", "kern", topicID)
	r.keep("stance_history-as-owner", raw)
	if h.Current != ownerStance.ID || !slices.ContainsFunc(h.Stances, func(st api.StanceRecord) bool { return strings.Contains(st.Position, "Each service owns") }) {
		t.Errorf("the shared topic's history for the owner = %s, want the owner's stance current beside the ticket's", raw)
	}
	b, raw := bundleOf("kyle", "kern")
	r.keep("get_bundle-as-owner", raw)
	servesOwner := false
	for _, s := range b.Stances {
		if s.TopicID == topicID && strings.Contains(s.Current, deletedWord) && slices.Contains(s.Evidence, cacheDoc) {
			servesOwner = true
		}
	}
	if !servesOwner {
		t.Errorf("get_bundle for the owner does not serve the owner's stance on %s: %s", topicID, raw)
	}
	code, topics := traverseRaw("kyle", "kern", "topics", "stances")
	r.keep("traverse-topics-stances-as-owner", []byte(topics))
	if code != http.StatusOK || !strings.Contains(topics, ownerStance.ID) {
		t.Errorf("traverse(topics, stances) for the owner = %d %s, want the owner's stance %s", code, topics, ownerStance.ID)
	}
	code, documents := traverseRaw("kyle", "kern", "documents")
	if code != http.StatusOK || !strings.Contains(documents, cacheDoc) || !strings.Contains(documents, lisbonDoc) || !strings.Contains(documents, ticketDoc) {
		t.Errorf("traverse(documents) for the owner = %d %s, want the conversation's documents and the ticket's", code, documents)
	}
	var decisionDoc, tripDoc l1.Stored
	mustAs("kyle", "kern", "get_l1", map[string]string{"id": cacheDoc}, &decisionDoc)
	mustAs("kyle", "kern", "get_l1", map[string]string{"id": lisbonDoc}, &tripDoc)
	if !strings.Contains(decisionDoc.RawText, "Owner: About the shared cache") || !strings.Contains(decisionDoc.RawText, "Kern: Noted") ||
		!slices.Equal(sortedCopy(decisionDoc.L0Refs), sortedCopy(exchange2)) || len(decisionDoc.ACL) != 1 {
		t.Errorf("the decision's document = %+v, want the exchange's two lines, cited by exactly its two events, with the owner's access list alone", decisionDoc.Document)
	}
	if !slices.Equal(sortedCopy(tripDoc.L0Refs), sortedCopy(lisbon)) || !strings.Contains(tripDoc.RawText, "Owner (interrupted): Also the Lisbon hotel") ||
		strings.Contains(tripDoc.RawText, "Good morning") || strings.Contains(tripDoc.RawText, deletedWord) {
		t.Errorf("the trip's document = %+v, want one document of its five messages, with the interrupted line marked", tripDoc.Document)
	}
	if code, raw := as("kyle", "kern", "get_l0", map[string]string{"id": ids["x2:owner"]}); code != http.StatusOK || !strings.Contains(string(raw), deletedWord) {
		t.Errorf("get_l0 of the decision's message for the owner = %d %s, want its words", code, raw)
	}
	if ownerWatch := <-watch("kyle", "", cursor); ownerWatch.code != http.StatusOK ||
		!slices.ContainsFunc(ownerWatch.out.Notifications, func(n api.Notification) bool { return n.Source == "kern" }) {
		t.Errorf("the owner's watch after the ticket = %d %s, want the conversation's events", ownerWatch.code, ownerWatch.raw)
	}

	annsWatch, scoutsWatch := watch("ann", "", cursor), watch("ann", "scout", cursor)
	conversationWords := []string{deletedWord, cacheDoc, lisbonDoc, ownerStance.ID, "conv-1", "evt:kern:", summaryWord, "Lisbon", "flights"}
	for _, reader := range []struct{ name, agent string }{{"ann", ""}, {"ann acting through scout", "scout"}} {
		who, agent := "ann", reader.agent
		leaks := func(call, body string) {
			t.Helper()
			for _, secret := range conversationWords {
				if strings.Contains(body, secret) {
					t.Errorf("%s: %s names %q: %s", reader.name, call, secret, body)
				}
			}
		}
		if got := search(who, agent, query("cache", nil)); !found(got, ticketDoc) {
			t.Errorf("%s: search %q does not find the readable ticket %s: %+v", reader.name, "cache", ticketDoc, got)
		}
		for _, q := range []string{summaryWord, deletedWord, "Lisbon", "platform", "flights"} {
			for _, extra := range []map[string]any{nil, window} {
				code, raw := as(who, agent, "search", query(q, extra))
				if code != http.StatusOK {
					t.Errorf("%s: search %q = %d %s", reader.name, q, code, raw)
				}
				leaks("search "+q, string(raw))
				var got api.SearchResult
				if json.Unmarshal(raw, &got) == nil && (len(got.Documents) != 0 || got.Truncated) {
					t.Errorf("%s: search %q found %s, want nothing (and no truncation they cannot account for)", reader.name, q, raw)
				}
			}
		}
		for _, id := range all {
			if code, raw := as(who, agent, "get_l0", map[string]string{"id": id}); code != http.StatusNotFound || strings.Contains(string(raw), deletedWord) {
				t.Errorf("%s: get_l0(%s) = %d %s, want 404", reader.name, id, code, raw)
			}
		}
		for _, id := range []string{cacheDoc, lisbonDoc} {
			if code, raw := as(who, agent, "get_l1", map[string]string{"id": id}); code != http.StatusNotFound {
				t.Errorf("%s: get_l1(%s) = %d %s, want 404", reader.name, id, code, raw)
			}
		}
		if code, raw := as(who, agent, "get_l1", map[string]string{"id": ticketDoc}); code != http.StatusOK {
			t.Errorf("%s: get_l1(%s) = %d %s, want the readable ticket", reader.name, ticketDoc, code, raw)
		}
		_, raw := bundleOf(who, agent)
		leaks("get_bundle", string(raw))
		for _, steps := range [][]string{{"topics", "stances"}, {"documents"}} {
			code, body := traverseRaw(who, agent, steps...)
			if code != http.StatusOK {
				t.Errorf("%s: traverse %v = %d %s", reader.name, steps, code, body)
			}
			leaks(fmt.Sprint("traverse ", steps), body)
			if steps[0] == "documents" && !strings.Contains(body, ticketDoc) {
				t.Errorf("%s: traverse(documents) = %s, want the readable ticket %s", reader.name, body, ticketDoc)
			}
		}
		code, theirs, raw := history(who, agent, topicID)
		leaks("stance_history", string(raw))
		if code != http.StatusOK || !slices.ContainsFunc(theirs.Stances, func(st api.StanceRecord) bool { return strings.Contains(st.Position, "Each service owns") }) {
			t.Errorf("%s: stance_history of the shared topic = %d %s, want the ticket's stance and nothing of the owner's", reader.name, code, raw)
		}
		if theirs.Current == ownerStance.ID {
			t.Errorf("%s: the topic's current stance is the owner's: %s", reader.name, raw)
		}
		missing := make([]string, len(all))
		for i := range missing {
			missing[i] = fmt.Sprintf("evt:kern:no-such-message-%d", i)
		}
		got, rawGot := coverage(who, agent, all)
		_, rawMissing := coverage(who, agent, missing)
		for i, id := range all {
			rawGot = bytes.ReplaceAll(rawGot, []byte(id), []byte(fmt.Sprintf("ID%d", i)))
			rawMissing = bytes.ReplaceAll(rawMissing, []byte(missing[i]), []byte(fmt.Sprintf("ID%d", i)))
		}
		for _, res := range got.Results {
			if res.State != "excluded" || len(res.Documents) != 0 {
				t.Errorf("%s: coverage of %s = %+v, want excluded", reader.name, res.EventID, res)
			}
		}
		if !bytes.Equal(rawGot, rawMissing) {
			t.Errorf("%s: coverage of the conversation's events = %s, want what ids that do not exist get: %s", reader.name, rawGot, rawMissing)
		}
	}
	for name, ch := range map[string]<-chan watched{"ann": annsWatch, "ann acting through scout": scoutsWatch} {
		w := <-ch
		if w.code != http.StatusOK {
			t.Errorf("%s: watch = %d %s", name, w.code, w.raw)
		}
		for _, n := range w.out.Notifications {
			t.Errorf("%s: watch delivered %+v, want none of the conversation", name, n)
		}
		for _, secret := range conversationWords {
			if strings.Contains(string(w.raw), secret) {
				t.Errorf("%s: watch names %q: %s", name, secret, w.raw)
			}
		}
	}
	t.Logf("EVIDENCE: ann, and scout acting for ann, saw the ticket %s on the shared topic %s and nothing of the conversation through search, get_l0, get_l1, get_bundle, traverse, stance_history, coverage and watch", ticketDoc, topicID)

	request := map[string]any{"event_ids": exchange2, "idempotency_id": "forget-x2"}
	var deleted api.Deleted
	raw = mustAs("kyle", "kern", "delete", request, &deleted)
	r.keep("delete", raw)
	if deleted.DeletionID == "" || !slices.Equal(sortedCopy(deleted.Deleted), sortedCopy(exchange2)) || !slices.Equal(deleted.Kept, []string{"ids", "times", "kinds"}) {
		t.Errorf("delete answered %s, want a deletion of both messages that keeps ids, times and kinds", raw)
	}
	var replayed api.Deleted
	code, again := r.callAs("kyle", "kern", true, "delete", request)
	if code != http.StatusOK || json.Unmarshal(again, &replayed) != nil || replayed.DeletionID != deleted.DeletionID ||
		!slices.Equal(sortedCopy(replayed.Deleted), sortedCopy(deleted.Deleted)) {
		t.Errorf("the same delete over MCP = %d %s, want the original answer %s", code, again, raw)
	}
	afterDelete, rawCovered := coverage("kyle", "kern", all)
	r.keep("coverage-after-delete", rawCovered)
	for _, res := range afterDelete.Results {
		if slices.Contains(exchange2, res.EventID) && (res.State != "excluded" || len(res.Documents) != 0) {
			t.Errorf("coverage of the deleted %s = %+v, want excluded", res.EventID, res)
		}
	}
	for _, id := range exchange2 {
		if code, raw := as("kyle", "kern", "get_l0", map[string]string{"id": id}); code != http.StatusNotFound {
			t.Errorf("get_l0 of the deleted %s = %d %s, want 404", id, code, raw)
		}
	}
	eventually("the deleted words leave every read", 3*time.Minute, func() string {
		if code, raw := as("kyle", "kern", "get_l1", map[string]string{"id": cacheDoc}); code != http.StatusNotFound {
			return fmt.Sprintf("get_l1(%s) = %d %s", cacheDoc, code, raw)
		}
		for _, q := range []string{deletedWord, summaryWord} {
			code, raw := as("kyle", "kern", "search", query(q, nil))
			var got api.SearchResult
			if code != http.StatusOK || json.Unmarshal(raw, &got) != nil || len(got.Documents) != 0 || strings.Contains(string(raw), deletedWord) {
				return fmt.Sprintf("search %q = %d %s", q, code, raw)
			}
		}
		if _, raw := bundleOf("kyle", "kern"); strings.Contains(string(raw), deletedWord) || strings.Contains(string(raw), cacheDoc) {
			return "get_bundle still serves it: " + string(raw)
		}
		code, h, raw := history("kyle", "kern", topicID)
		if code != http.StatusOK || strings.Contains(string(raw), deletedWord) {
			return fmt.Sprintf("stance_history = %d %s", code, raw)
		}
		for _, st := range h.Stances {
			if st.ID == h.Current && strings.Contains(st.Position, deletedWord) {
				return "the current stance quotes it: " + st.Position
			}
		}
		return ""
	})
	_, h, raw = history("kyle", "kern", topicID)
	r.keep("stance_history-after-delete", raw)
	t.Logf("EVIDENCE: after the deletion's rebuild %q is in none of search, get_l0, get_l1, get_bundle or stance_history; the shared topic stands at %q", deletedWord, h.Current)
	stillThere, _ := coverage("kyle", "kern", lisbon)
	for _, res := range stillThere.Results {
		if res.State != "covered" || !slices.Equal(res.Documents, []string{lisbonDoc}) {
			t.Errorf("coverage of the trip's %s after the deletion = %+v, want covered by %s", res.EventID, res, lisbonDoc)
		}
	}
	if code, raw := as("kyle", "kern", "get_l1", map[string]string{"id": lisbonDoc}); code != http.StatusOK {
		t.Errorf("get_l1 of the trip after the deletion = %d %s", code, raw)
	}

	before = eventCount("kern")
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"as it was", message("x2", "owner", script[4].text, 10, false)},
		{"rewritten", message("x2", "owner", "About the shared cache: I changed my mind about "+deletedWord+".", 10, false)},
		{"the answer", message("x2", "assistant", script[5].text, 10, false)},
	} {
		code, raw := hook(c.body)
		if code != http.StatusGone || !strings.Contains(raw, `"deleted"`) {
			t.Errorf("re-posting the deleted message %s = %d %s, want 410 deleted", c.name, code, raw)
		}
	}
	if after := eventCount("kern"); after != before {
		t.Errorf("re-posts of deleted messages wrote %d events", after-before)
	}
	reread, _ := coverage("kyle", "kern", exchange2)
	for _, res := range reread.Results {
		if res.State != "excluded" {
			t.Errorf("coverage of %s after the re-posts = %+v, want excluded", res.EventID, res)
		}
	}
	for _, id := range exchange2 {
		if code, raw := as("kyle", "kern", "get_l0", map[string]string{"id": id}); code != http.StatusNotFound {
			t.Errorf("get_l0 of %s after the re-posts = %d %s, want 404", id, code, raw)
		}
	}
	t.Logf("EVIDENCE: re-posts of the deleted exchange answered 410 deleted and wrote no event (%d events in the source)", before)

	t.Logf("EVIDENCE: the stub answered %v; no real model or Jev is reachable from this test", stub.counts())
	for _, name := range []string{"conversation_topics", "l1_conversation_segment", "l1_issue", "l2_assertions", "judgment"} {
		if stub.counts()[name] == 0 {
			t.Errorf("the stub was never asked for %s, so that stage ran on nothing", name)
		}
	}
	stop()
	if t.Failed() {
		t.Logf("hearsay all log:\n%s", r.stderr.String())
	}
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}
