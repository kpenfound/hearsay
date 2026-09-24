//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func (w *world) operate(t *testing.T, req l2.OperationRequest) l2.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req.Principal = "kyle"
	op, err := l2.Operate(ctx, w.pool, repo(w.src), req)
	if err != nil {
		t.Fatalf("Operate(%+v) = %v", req, err)
	}
	return op
}

// stances is the ids of a history's stances.
func stances(h api.History) []string {
	out := make([]string, len(h.Stances))
	for i, st := range h.Stances {
		out[i] = st.ID
	}
	return out
}

func record(h api.History, id string) api.StanceRecord {
	for _, st := range h.Stances {
		if st.ID == id {
			return st
		}
	}
	return api.StanceRecord{}
}

// stance_history, the bundle and assert serve topics as the topic ledger makes
// them, through either side of a merge, and filtered for the reader.
func TestReadsFollowTheTopicLedger(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	graph := l2.New(w.pool)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}}
	topic := func(doc, name string) l2.Topic {
		t.Helper()
		tp := l2.Topic{ID: l2.TopicID(w.src, doc, 0, name), Scope: w.src, Name: name, About: []string{w.scope}, ACL: public, OpenedBy: doc}
		if _, err := graph.OpenTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
		return tp
	}
	stance := func(tp l2.Topic, doc, position string, hour int) l2.Stance {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		st, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(tp.ID, doc, position, at, l2.TierInferred), TopicID: tp.ID, Position: position,
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: public,
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	d40 := w.putIn(w.scope, w.project+"#40", l1.KindIssue, 40, "The cache is per service.", public)
	d41 := w.putIn(w.scope, w.project+"#41", l1.KindIssue, 41, "The cache moves to the edge.", public)
	d42 := w.putIn(w.scope, w.project+"#42", l1.KindIssue, 42, "The cache belongs to the platform team.", private)
	d43 := w.putIn(w.scope, w.project+"#43", l1.KindIssue, 43, "The platform team hands the cache to each service.", public)
	a := topic(d40, "who owns the cache")
	b := topic(d41, "where the cache lives")
	a1 := stance(a, d40, "each service", 40)
	b1 := stance(b, d42, "the platform team", 42)
	b2 := stance(b, d43, "each service, from the platform team", 43)
	callers := []api.Caller{kyle, sam}
	before := map[string]string{}
	for _, c := range callers {
		for _, id := range []string{a.ID, b.ID} {
			before[c.Principal+id] = string(w.http(t, c, "stance_history", map[string]any{"topic": id}))
		}
	}

	// Merged: one history through either id, naming the merge, standing on
	// every stance of both.
	merged := w.operate(t, l2.OperationRequest{Kind: l2.OperationMerge, Into: a.ID, From: b.ID})
	throughA := w.http(t, kyle, "stance_history", map[string]any{"topic": a.ID})
	throughB := w.http(t, kyle, "stance_history", map[string]any{"topic": b.ID})
	if string(throughA) != string(throughB) {
		t.Errorf("stance_history through the two sides of the merge differs:\n%s\n%s", throughA, throughB)
	}
	h := w.history(t, kyle, b.ID)
	if h.ID != a.ID || h.Topic != a.Name || h.Current != b2.ID || len(h.Operations) != 1 || h.Operations[0].ID != merged.ID ||
		h.Operations[0].Kind != "merge" || !slices.Equal(h.Operations[0].Topics, []string{a.ID, b.ID}) ||
		!slices.Equal(stances(h), []string{a1.ID, b1.ID, b2.ID}) {
		t.Errorf("kyle's stance_history through the merged-away topic = %+v, want the merged topic", h)
	}
	hs := w.history(t, sam, b.ID)
	if !slices.Equal(stances(hs), []string{a1.ID, b2.ID}) || record(hs, b2.ID).Supersedes != "" || hs.Current != b2.ID {
		t.Errorf("sam's stance_history = %+v, want the private stance and the edge to it left out", hs)
	}
	body, err := w.calls.Call(ctx, sam, "get_bundle", mustJSON(t, map[string]any{"scope": w.scope}))
	if err != nil {
		t.Fatal(err)
	}
	var onCache []string
	for _, s := range decodeBundle(t, body).Stances {
		if s.TopicID == a.ID || s.TopicID == b.ID {
			onCache = append(onCache, s.TopicID+" "+s.Current)
		}
	}
	if !slices.Equal(onCache, []string{a.ID + " " + b2.Position}) {
		t.Errorf("sam's bundle serves %v, want the merged topic once, at %s", onCache, b2.ID)
	}

	// An agent asserting on the merged-away topic writes on the topic it went
	// into.
	var asserted api.Asserted
	if err := json.Unmarshal(w.http(t, shedKyle, "assert", map[string]any{"topic": b.ID, "position": "the edge team", "evidence": []string{d41}}), &asserted); err != nil {
		t.Fatal(err)
	}
	ev, err := l0.New(w.pool).Get(ctx, asserted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if as, err := l2.AssertionOf(ev); err != nil || as.Topic != a.ID {
		t.Errorf("the assertion = %+v, %v, want it on %s", as, err, a.ID)
	}
	key, _ := w.assertJob(t, asserted.ID)
	if _, err := assertworker.AppendAssertion(ctx, w.pool, config.Authority{}, asserted.ID, key); err != nil {
		t.Fatal(err)
	}
	asStance, err := graph.Stance(ctx, l2.AssertionStanceID(a.ID, asserted.ID))
	if err != nil || asStance.TopicID != a.ID {
		t.Fatalf("the asserted stance = %+v, %v, want it on %s", asStance, err, a.ID)
	}

	// Split: the private stance alone is a topic kyle reads and sam does not,
	// and the edge the split cut is shown to kyle from both ends and to sam
	// from neither.
	split := w.operate(t, l2.OperationRequest{Kind: l2.OperationSplit, Topic: a.ID, Name: "the platform team's cache", Stances: []string{b1.ID}})
	s := split.Topics[1]
	hs = w.history(t, kyle, s)
	if hs.ID != s || hs.Topic != "the platform team's cache" || !slices.Equal(stances(hs), []string{b1.ID}) ||
		!slices.Equal(record(hs, b1.ID).SupersededBy, []api.StanceRef{{ID: b2.ID, Topic: a.ID}}) {
		t.Errorf("kyle's stance_history of the split's topic = %+v, want b1 alone, superseded by b2 on %s", hs, a.ID)
	}
	ha := w.history(t, kyle, a.ID)
	if got := record(ha, b2.ID); got.Supersedes != b1.ID || got.SupersedesTopic != s {
		t.Errorf("kyle's b2 = %+v, want it to supersede b1 on the split's topic", got)
	}
	if len(ha.Operations) != 2 || ha.Operations[1].ID != split.ID {
		t.Errorf("kyle's history of a names %+v, want the merge and the split", ha.Operations)
	}
	if status, got := w.post(t, "/v1/stance_history", sam, `{"topic":"`+s+`"}`); status != http.StatusNotFound {
		t.Errorf("sam's stance_history of the private split = %d %s, want 404", status, got)
	}
	if got := record(w.history(t, sam, a.ID), b2.ID); got.Supersedes != "" || got.SupersedesTopic != "" {
		t.Errorf("sam's b2 = %+v, want no edge to a stance sam may not read", got)
	}

	// Undone, without the agent's stance each read is as it was.
	w.operate(t, l2.OperationRequest{Kind: l2.OperationUndo, Undoes: split.ID})
	w.operate(t, l2.OperationRequest{Kind: l2.OperationUndo, Undoes: merged.ID})
	if _, err := w.pool.Exec(ctx, `DELETE FROM l2_stances WHERE id = $1`, asStance.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range callers {
		for _, id := range []string{a.ID, b.ID} {
			if got := string(w.http(t, c, "stance_history", map[string]any{"topic": id})); got != before[c.Principal+id] {
				t.Errorf("%s's stance_history of %s after the undos = %s, want %s", c.Principal, id, got, before[c.Principal+id])
			}
		}
	}
}
