//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

var (
	shedSam  = api.Caller{Principal: "sam", Agent: "shed"}
	peekKyle = api.Caller{Principal: "kyle", Agent: "peek"}
)

// lockTopic is the world's public topic, where the merged pull request is the
// ratified current stance.
func (w *world) lockTopic() string { return l2.TopicID(w.src, w.issue, 0, "where the lock lives") }

// assertJob is the serial key of the assert job enqueued for an event, and how
// many there are.
func (w *world) assertJob(t *testing.T, eventID string) (string, int) {
	t.Helper()
	var key string
	var n int
	err := w.pool.QueryRow(t.Context(), `SELECT coalesce(min(serial_key), ''), count(*) FROM queue_job WHERE kind = $1 AND target_id = $2`,
		l2.AssertKindName, eventID).Scan(&key, &n)
	if err != nil {
		t.Fatal(err)
	}
	return key, n
}

func (w *world) history(t *testing.T, caller api.Caller, topic string) api.History {
	t.Helper()
	var h api.History
	if err := json.Unmarshal(w.http(t, caller, "stance_history", map[string]any{"topic": topic}), &h); err != nil {
		t.Fatal(err)
	}
	return h
}

// The acceptance criteria: an agent's assertion is an L0 event, the job that
// appends it is enqueued under the topic's scope, and once the worker has run
// it is a stance in stance_history — by the agent, citing every document, and
// not ratified. The same request over MCP is the same answer, and writes
// nothing more.
func TestAnAgentAssertsAStanceThatStanceHistoryServes(t *testing.T) {
	w := newWorld(t)
	topic := w.lockTopic()
	args := map[string]any{"topic": topic, "position": "  The engine takes the lock, and the queue never does.  ", "evidence": []string{w.pr, w.issue, w.pr}}

	overHTTP := w.http(t, shedKyle, "assert", args)
	var asserted api.Asserted
	if err := json.Unmarshal(overHTTP, &asserted); err != nil || asserted.ID == "" {
		t.Fatalf("assert = %s, %v; want an event id", overHTTP, err)
	}
	ev, err := l0.New(w.pool).Get(t.Context(), asserted.ID)
	if err != nil {
		t.Fatalf("the assertion event: %v", err)
	}
	if ev.Kind != connector.KindAssertion || ev.Source != api.AuditSource || ev.Payload.Author == nil ||
		ev.Payload.Author.Kind != connector.IdentityAgent || ev.Payload.Author.NativeID != "shed" {
		t.Errorf("the event = %+v, want an assertion under %s authored by the agent", ev, api.AuditSource)
	}
	as, err := l2.AssertionOf(ev)
	if err != nil {
		t.Fatal(err)
	}
	if want := (l2.Assertion{Topic: topic, Position: "The engine takes the lock, and the queue never does.",
		Evidence: l2.SortedEvidence([]string{w.issue, w.pr}), Agent: "shed", Principal: "kyle"}); !assertionEqual(as, want) {
		t.Errorf("the event carries %+v, want %+v", as, want)
	}
	// The event fails closed, as an audit event does: the stance is how the
	// position is read.
	if status, _ := w.post(t, "/v1/get_l0", kyle, `{"id":"`+asserted.ID+`"}`); status != http.StatusNotFound {
		t.Errorf("get_l0 of the assertion event = %d, want 404", status)
	}

	// A retry, over the other interface: the same bytes, one event, one job.
	overMCP, isError := w.mcp(t, shedKyle, "assert", args)
	if isError || !bytes.Equal(overHTTP, overMCP) {
		t.Errorf("the same assert over MCP = %s (isError %v), want %s", overMCP, isError, overHTTP)
	}
	var events int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l0_events WHERE id = $1`, asserted.ID).Scan(&events); err != nil || events != 1 {
		t.Errorf("assertion events = %d, %v, want 1", events, err)
	}
	key, jobs := w.assertJob(t, asserted.ID)
	if key != w.src || jobs != 1 {
		t.Errorf("assert jobs = %d under %q, want 1 under the topic's scope %q", jobs, key, w.src)
	}

	// Nothing is in L2 until the worker runs; then the stance is there, once,
	// however often its job runs.
	if h := w.history(t, kyle, topic); len(h.Stances) != 3 {
		t.Errorf("stance_history before the worker = %d stances, want the world's 3", len(h.Stances))
	}
	for range 2 {
		if _, err := assertworker.AppendAssertion(t.Context(), w.pool, config.Authority{}, asserted.ID, key); err != nil {
			t.Fatalf("AppendAssertion() = %v", err)
		}
	}
	h := w.history(t, kyle, topic)
	if len(h.Stances) != 4 {
		t.Fatalf("stance_history = %+v, want the world's 3 and the agent's", h.Stances)
	}
	got := h.Stances[len(h.Stances)-1]
	if got.Author != "shed" || got.Position != as.Position || got.RecordedTier != string(l2.TierInferred) ||
		!slices.Equal(got.Evidence, as.Evidence) || got.Judgement != nil {
		t.Errorf("the agent's stance = %+v, want shed's position, inferred, citing %v", got, as.Evidence)
	}
	// The world's stances were stated weeks before the assertion, outside the
	// contested window, so the agent's is current — and inferred: an agent
	// does not ratify on its own under the default policy. That it ranks last
	// within the window is internal/l2's and the assertion worker's to show.
	if h.Current != got.ID || h.Tier != string(l2.TierInferred) {
		t.Errorf("the topic stands at %s (%s), want the agent's stance, inferred", h.Current, h.Tier)
	}

	// Sam may not read the private stance on the topic, but may read the
	// agent's: every document it cites is public.
	if h := w.history(t, sam, topic); !slices.ContainsFunc(h.Stances, func(s api.StanceRecord) bool { return s.ID == got.ID }) {
		t.Errorf("sam's history = %+v, want the agent's stance in it", h.Stances)
	}
}

func assertionEqual(a, b l2.Assertion) bool {
	return a.Topic == b.Topic && a.Position == b.Position && slices.Equal(a.Evidence, b.Evidence) && a.Agent == b.Agent && a.Principal == b.Principal
}

// The acceptance criteria: a person, an observer, and a topic or evidence the
// effective principal cannot read are refused; the refusals for a missing and
// an unreadable id are one answer, so neither says which; every piece of
// evidence is checked; and nothing is written for a refusal.
func TestAssertRefusesWithoutRevealingWhatExists(t *testing.T) {
	w := newWorld(t)
	topic := w.lockTopic()
	hidden := l2.TopicID(w.src, w.secret, 0, "the customer")
	ok := func(topic string, evidence ...string) map[string]any {
		return map[string]any{"topic": topic, "position": "the engine takes the lock", "evidence": evidence}
	}
	notYours := `{"error":"the topic and the evidence are not all ones you may read"}`

	tests := []struct {
		name   string
		caller api.Caller
		args   map[string]any
		status int
		body   string
	}{
		{"a person with no agent", kyle, ok(topic, w.issue), http.StatusForbidden, ""},
		{"an observer", peekKyle, ok(topic, w.issue), http.StatusForbidden, ""},
		{"a topic that does not exist", shedKyle, ok("topic:nothing", w.issue), http.StatusNotFound, notYours},
		{"a topic the person may not read", shedSam, ok(hidden, w.issue), http.StatusNotFound, notYours},
		{"evidence that does not exist", shedKyle, ok(topic, w.issue, "l1:nobody:nothing"), http.StatusNotFound, notYours},
		{"one piece of evidence of several the person may not read", shedSam, ok(topic, w.issue, w.pr, w.secret), http.StatusNotFound, notYours},
		{"no evidence", shedKyle, ok(topic), http.StatusBadRequest, ""},
		{"an empty evidence id", shedKyle, ok(topic, w.issue, " "), http.StatusBadRequest, ""},
		{"no position", shedKyle, map[string]any{"topic": topic, "position": "  ", "evidence": []string{w.issue}}, http.StatusBadRequest, ""},
		{"a position over the bound", shedKyle, map[string]any{"topic": topic, "position": strings.Repeat("x", l2.MaxPosition+1), "evidence": []string{w.issue}}, http.StatusBadRequest, ""},
		{"an argument it does not take", shedKyle, map[string]any{"topic": topic, "position": "p", "evidence": []string{w.issue}, "tier": "ratified"}, http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.args)
			status, overHTTP := w.post(t, "/v1/assert", tt.caller, string(body))
			if status != tt.status || tt.body != "" && string(overHTTP) != tt.body {
				t.Fatalf("assert = %d %s, want %d %s", status, overHTTP, tt.status, tt.body)
			}
			overMCP, isError := w.mcp(t, tt.caller, "assert", tt.args)
			if !isError || !bytes.Equal(overHTTP, overMCP) {
				t.Errorf("over MCP = %s (isError %v), want the same refusal as over HTTP", overMCP, isError)
			}
			// Nothing was written under the id the request would have had.
			position, _ := tt.args["position"].(string)
			evidence, _ := tt.args["evidence"].([]string)
			topic, _ := tt.args["topic"].(string)
			would := connector.EventID(api.AuditSource, l2.Assertion{Topic: topic, Position: strings.TrimSpace(position),
				Evidence: l2.SortedEvidence(evidence), Agent: tt.caller.Agent, Principal: tt.caller.Principal}.NativeID())
			if _, err := l0.New(w.pool).Get(t.Context(), would); !errors.Is(err, l0.ErrNotFound) {
				t.Errorf("a refused assertion's event: %v, want none", err)
			}
		})
	}
}
