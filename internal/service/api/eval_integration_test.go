//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/agent"
	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/service/api"
)

// The evaluation metrics read what the API and the agent source actually
// write: sessions driven through bundles, handle calls and next actions, then
// `eval.Compute` over the world's scope.
//
// Session one is served a bundle with a flagged conflict, follows a stance's
// evidence, searches for something the bundle did not serve, and proceeds,
// judging the conflict real. Session two is served the same bundle, follows
// nothing, and asks, judging no conflict. A third bundle is served with no
// session.
func TestEvalReadsSessionTrails(t *testing.T) {
	w := newWorld(t)
	since := time.Now().UTC().Add(-time.Second)
	src := repo(w.src).Sources[0]
	c, err := agent.New(src, repo(w.src).Principals, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.Handler(connector.NewGate(l0.New(w.pool), src.ID, c.Describe(), connector.NewAllowlist(src))))
	defer srv.Close()
	started := time.Now().UTC().Add(-time.Minute)
	post := func(session string, kind connector.Kind, fields map[string]any) {
		t.Helper()
		body := map[string]any{"on_behalf_of": "kyle", "session": session, "kind": kind, "started_at": started, "time": time.Now().UTC()}
		if kind == connector.KindAgentSession {
			body["time"] = started
		}
		for k, v := range fields {
			body[k] = v
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, bytes.NewReader(mustJSON(t, body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-shed-api-credential")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("POST %s = %d", kind, resp.StatusCode)
		}
	}
	directive := connector.Event{Source: w.src, NativeID: "eval-conflict", Kind: connector.KindMessage, Time: day,
		Payload: connector.Payload{Artifact: "eval-conflict", Container: connector.Container{Kind: connector.ContainerChannel, NativeID: w.project},
			Thread: w.project + "#12", Text: "where the lock lives",
			Author:   &connector.Identity{Source: w.src, Kind: connector.IdentityUser, NativeID: kyleNode},
			Mentions: []connector.Identity{{Source: w.src, Kind: connector.IdentityBot, NativeID: "shed-native", Handle: "shed[bot]"}}},
		ACL: connector.ACL{{Kind: connector.ACLPublic}}}
	if _, err := l0.New(w.pool).Append(t.Context(), directive); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"scope": w.scope, "directive": connector.EventID(w.src, directive.NativeID)}

	one := api.Caller{Principal: "kyle", Agent: "shed", Session: "eval-one"}
	post(one.Session, connector.KindAgentSession, map[string]any{"phase": "start"})
	b := decodeBundle(t, w.http(t, one, "get_bundle", args))
	if len(b.Conflicts) == 0 || len(b.Stances) == 0 {
		t.Fatalf("the served bundle has %d conflicts and %d stances; the test needs both", len(b.Conflicts), len(b.Stances))
	}
	evidence := b.Stances[0].Evidence[0]
	w.http(t, one, "get_l1", map[string]any{"id": evidence})
	w.http(t, one, "search", map[string]any{"query": "nothing in this world says zebra"})
	post(one.Session, connector.KindNextAction, map[string]any{"next": "1", "scope": w.scope, "action": "proceeded",
		"verdicts": []agent.Verdict{{TopicID: b.Conflicts[0].TopicID, Verdict: "real"}}})

	two := api.Caller{Principal: "kyle", Agent: "shed", Session: "eval-two"}
	post(two.Session, connector.KindAgentSession, map[string]any{"phase": "start"})
	w.http(t, two, "get_bundle", args)
	post(two.Session, connector.KindNextAction, map[string]any{"next": "1", "scope": w.scope, "action": "asked"})

	w.http(t, shedKyle, "get_bundle", args)

	report, err := eval.Compute(t.Context(), w.pool, config.Authority{}, eval.Options{Since: since, Until: time.Now().UTC().Add(time.Minute), Scope: w.scope})
	if err != nil {
		t.Fatal(err)
	}
	drill := report.DrillDown
	if drill.Bundles != 2 || drill.WithoutSession != 1 {
		t.Errorf("bundles = %d with a session and %d without, want 2 and 1", drill.Bundles, drill.WithoutSession)
	}
	// What each section of the bundle served, as the audit records it: the
	// evidence followed may be served by other sections too.
	served := map[string][]string{}
	for _, e := range b.Scope.Entities {
		served["scope.entities"] = append(served["scope.entities"], e.ID, e.L1)
	}
	for _, a := range b.Anchors {
		served["anchors"] = append(served["anchors"], a.L1)
	}
	for _, st := range b.Stances {
		served["stances"] = append(append(served["stances"], st.TopicID), st.Evidence...)
	}
	for _, item := range b.Recent.Items {
		served["recent"] = append(served["recent"], item.L1)
	}
	for _, q := range b.OpenQuestions {
		served["open_questions"] = append(served["open_questions"], q.Evidence...)
	}
	for _, conflict := range b.Conflicts {
		served["conflicts"] = append(served["conflicts"], conflict.TopicID)
	}
	for _, sec := range drill.Sections {
		wantServed, wantFollowed := 0, 0
		if len(served[sec.Section]) > 0 {
			wantServed = 2
		}
		if slices.Contains(served[sec.Section], evidence) {
			wantFollowed = 1
		}
		if sec.Served != wantServed || sec.Followed != wantFollowed {
			t.Errorf("section %s followed in %d of %d bundles, want %d of %d", sec.Section, sec.Followed, sec.Served, wantFollowed, wantServed)
		}
	}
	if s := drill.LookedBeyond.Search; s.Calls != 1 || s.Beyond != 1 {
		t.Errorf("searches = %+v, want the one that looked beyond the bundle", s)
	}
	flagged := 2 * len(b.Conflicts)
	flags := report.Conflicts
	if flags.Flagged != flagged || flags.Verdicts != 1 || flags.Real != 1 || flags.Spurious != 0 ||
		flags.Precision == nil || *flags.Precision != 1 || flags.Coverage == nil || *flags.Coverage != 1/float64(flagged) {
		t.Errorf("conflict flags = %+v, want %d flagged and one real verdict", flags, flagged)
	}
	next := report.NextActions
	if next.Actions != 2 || next.Asked.Count != 1 || next.Proceeded.Count != 1 || next.Asserted.Count != 0 || next.Unattributed != 0 {
		t.Errorf("next actions = %+v, want one asked and one proceeded, both after a bundle", next)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"where the lock lives", "zebra"} {
		if bytes.Contains(raw, []byte(text)) {
			t.Errorf("the report holds %q", text)
		}
	}
}
