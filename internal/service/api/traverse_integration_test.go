//go:build integration

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

// both uses the world's credentials and checks the shared HTTP/MCP bytes.
func (w *world) both(t *testing.T, caller api.Caller, call string, args any) (answer, answer) {
	t.Helper()
	status, body := w.post(t, "/v1/"+call, caller, string(mustJSON(t, args)))
	mcp, isError := w.mcp(t, caller, call, args)
	return answer{status: status, body: string(body)}, answer{status: 200, isError: isError, body: string(mcp)}
}

func traverseArgs(typ, id string, steps ...map[string]any) map[string]any {
	return map[string]any{"start": []map[string]string{{"type": typ, "id": id}}, "steps": steps}
}
func walk(t *testing.T, w *world, caller api.Caller, args any) (l3.Subgraph, string) {
	t.Helper()
	h, m := w.both(t, caller, "traverse", args)
	if h.status != 200 || m.isError || h.body != m.body {
		t.Fatalf("traverse HTTP=%+v MCP=%+v", h, m)
	}
	var out l3.Subgraph
	if err := json.Unmarshal([]byte(h.body), &out); err != nil {
		t.Fatal(err)
	}
	seen := map[l3.NodeRef]bool{}
	for _, n := range out.Nodes {
		if seen[n.NodeRef] {
			t.Fatalf("duplicate node: %+v", n)
		}
		seen[n.NodeRef] = true
		for _, line := range []string{n.Line, n.Name, n.Position} {
			if len(line) > 200 {
				t.Fatalf("long line: %s", line)
			}
		}
	}
	for _, e := range out.Edges {
		if !seen[e.From] || !seen[e.To] {
			t.Fatalf("edge leaves returned nodes: %+v", e)
		}
	}
	return out, h.body
}
func nodeIDs(g l3.Subgraph) []string {
	ids := []string{}
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	return ids
}
func traverseEntity(t *testing.T, w *world, id string, parents ...string) {
	t.Helper()
	if err := l2.New(w.pool).PutEntity(t.Context(), l2.Entity{ID: id, Name: id, Type: l2.TypeTrackerItem, PartOf: parents, Origin: l2.OriginReference}); err != nil {
		t.Fatal(err)
	}
}
func traverseTopic(t *testing.T, w *world, doc, name, entity string) l2.Topic {
	t.Helper()
	tp := l2.Topic{ID: l2.TopicID(w.src, doc, 0, name), Name: name, Scope: w.src, About: []string{entity}, OpenedBy: doc, ACL: connector.ACL{{Kind: connector.ACLPublic}}}
	if _, err := l2.New(w.pool).OpenTopic(t.Context(), tp); err != nil {
		t.Fatal(err)
	}
	return tp
}
func traverseStance(t *testing.T, w *world, tp l2.Topic, doc, position string, hour int) l2.Stance {
	t.Helper()
	at := day.Add(time.Duration(hour) * time.Hour)
	st, _, err := l2.New(w.pool).AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(tp.ID, doc, position, at, l2.TierInferred), TopicID: tp.ID, Position: position, Author: "kyle", StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: connector.ACL{{Kind: connector.ACLPublic}}}, at)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTraverseDownwardStateAndTiers(t *testing.T) {
	w := newWorld(t)
	traverseEntity(t, w, w.scope)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	want := map[string]bool{}
	for i, state := range []string{"started", "completed", "started"} {
		artifact := fmt.Sprintf("%s#%d", w.project, 100+i)
		id := "tracker:" + w.src + ":" + artifact
		traverseEntity(t, w, id, w.scope)
		doc := w.putIn(id, artifact, l1.KindIssue, 10+i, strings.Repeat("long summary ", 40), public)
		if _, err := w.pool.Exec(t.Context(), `UPDATE l1_docs SET state=$1 WHERE id=$2`, state, doc); err != nil {
			t.Fatal(err)
		}
		tp := traverseTopic(t, w, doc, "child decision "+state, id)
		traverseStance(t, w, tp, doc, strings.Repeat("position ", 50), 10+i)
		if state == "started" {
			want[id] = true
			want[tp.ID] = true
		}
	}
	// A child without a subject never matches a state filter.
	traverseEntity(t, w, "code:"+w.src+"-no-subject", w.scope)
	args := traverseArgs("entity", w.scope, map[string]any{"edge": "has_part", "where": map[string]any{"state": "started"}}, map[string]any{"edge": "topics", "where": map[string]any{"tier": "inferred"}})
	out, body := walk(t, w, sam, args)
	if len(out.Nodes) != 5 || out.Truncated {
		t.Fatalf("downward=%s", body)
	}
	for _, n := range out.Nodes[1:] {
		if !want[n.ID] {
			t.Fatalf("unexpected node: %+v", n)
		}
		if n.Type == "topic" {
			var history api.History
			json.Unmarshal(w.http(t, sam, "stance_history", map[string]any{"topic": n.ID}), &history)
			if n.Tier != history.Tier || n.Tier != "inferred" {
				t.Fatalf("tier=%s, history=%s", n.Tier, history.Tier)
			}
		}
		if n.Type == "entity" && (n.State != "started" || len(n.Line) > 200 || n.LastActivity == "") {
			t.Fatalf("entity=%+v", n)
		}
	}
	for range 2 {
		_, again := walk(t, w, sam, args)
		if again != body {
			t.Fatal("repeat differs")
		}
	}
	for _, e := range out.Edges {
		if e.Edge == "part_of" && e.To.ID != w.scope {
			t.Fatalf("inverse edge not stored direction: %+v", e)
		}
	}
}

func TestTraverseLedgerMergeSplitAndSupersession(t *testing.T) {
	w := newWorld(t)
	traverseEntity(t, w, w.scope)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	d1 := w.putIn(w.scope, w.project+"#40", l1.KindIssue, 40, "first decision", public)
	d2 := w.putIn(w.scope, w.project+"#41", l1.KindIssue, 41, "next decision", public)
	a := traverseTopic(t, w, d1, "first", w.scope)
	b := traverseTopic(t, w, d2, "second", w.scope)
	_ = traverseStance(t, w, a, d1, "old", 40)
	b1 := traverseStance(t, w, b, d2, "new", 41)
	merge := w.operate(t, l2.OperationRequest{Kind: l2.OperationMerge, From: b.ID, Into: a.ID})
	for _, args := range []map[string]any{
		traverseArgs("entity", w.scope, map[string]any{"edge": "topics"}),
		traverseArgs("stance", b1.ID, map[string]any{"edge": "topic"}),
		traverseArgs("topic", b.ID, map[string]any{"edge": "stances"}),
	} {
		g, body := walk(t, w, kyle, args)
		if slices.Contains(nodeIDs(g), b.ID) || !slices.Contains(nodeIDs(g), a.ID) {
			t.Fatalf("merged identity: %s", body)
		}
	}
	w.operate(t, l2.OperationRequest{Kind: l2.OperationUndo, Undoes: merge.ID})
	// A split cuts b2's supersession edge to b1; the read must follow it both ways.
	d3 := w.putIn(w.scope, w.project+"#42", l1.KindIssue, 42, "third decision", public)
	b2 := traverseStance(t, w, b, d3, "newer", 42)
	split := w.operate(t, l2.OperationRequest{Kind: l2.OperationSplit, Topic: b.ID, Stances: []string{b2.ID}, Name: "split decision"})
	for _, tt := range []struct{ edge, start, want string }{{"superseded_by", b1.ID, b2.ID}, {"supersedes", b2.ID, b1.ID}} {
		g, body := walk(t, w, kyle, traverseArgs("stance", tt.start, map[string]any{"edge": tt.edge, "depth": "1..6"}))
		if !slices.Contains(nodeIDs(g), tt.want) || len(g.Edges) != 1 || g.Edges[0].From.ID != b2.ID || g.Edges[0].To.ID != b1.ID {
			t.Fatalf("cross split: %s", body)
		}
	}
	g, body := walk(t, w, kyle, traverseArgs("entity", w.scope, map[string]any{"edge": "topics"}))
	if !slices.Contains(nodeIDs(g), split.Topics[1]) {
		t.Fatalf("missing split: %s", body)
	}
}

func TestTraverseHiddenNodesEqualAbsentAndNotFound(t *testing.T) {
	w := newWorld(t)
	traverseEntity(t, w, w.scope)
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}}
	hiddenDoc := w.putIn(w.scope, w.project+"#60", l1.KindIssue, 60, "UNREADABLE CONTENT", private)
	hidden := traverseTopic(t, w, hiddenDoc, "UNREADABLE TOPIC", w.scope)
	// This stance's evidence is public, but reaching it via its hidden topic is forbidden.
	visibleStance := traverseStance(t, w, hidden, w.issue, "VISIBLE ONLY THROUGH HIDDEN TOPIC", 61)
	args := traverseArgs("entity", w.scope, map[string]any{"edge": "topics"}, map[string]any{"edge": "stances"})
	_, present := walk(t, w, sam, args)
	for _, secret := range []string{hidden.ID, hiddenDoc, visibleStance.ID, "UNREADABLE", "VISIBLE ONLY"} {
		if strings.Contains(present, secret) {
			t.Fatalf("leak %q: %s", secret, present)
		}
	}
	for _, tt := range []struct{ typ, id, edge string }{{"topic", hidden.ID, "stances"}, {"document", hiddenDoc, "scope"}, {"stance", visibleStance.ID, "topic"}} {
		a, am := w.both(t, sam, "traverse", traverseArgs(tt.typ, tt.id, map[string]any{"edge": tt.edge}))
		b, bm := w.both(t, sam, "traverse", traverseArgs(tt.typ, "nonexistent", map[string]any{"edge": tt.edge}))
		if a.status != 404 || b.status != 404 || a.body != b.body || a.body != am.body || b.body != bm.body || !am.isError || !bm.isError {
			t.Fatalf("not-found mismatch: %+v %+v", a, b)
		}
	}
	if _, err := w.pool.Exec(t.Context(), `DELETE FROM l2_stances WHERE topic_id=$1`, hidden.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(t.Context(), `DELETE FROM l2_topics WHERE id=$1`, hidden.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(t.Context(), `DELETE FROM l1_docs WHERE id=$1`, hiddenDoc); err != nil {
		t.Fatal(err)
	}
	_, absent := walk(t, w, sam, args)
	if present != absent {
		t.Fatalf("hidden-present != absent\n%s\n%s", present, absent)
	}
	// A hidden document also never consumes a visible limit or changes truncation.
	docsArgs := traverseArgs("entity", w.scope, map[string]any{"edge": "documents"})
	docsArgs["limit"] = 2
	_, before := walk(t, w, sam, docsArgs)
	hiddenDoc = w.putIn(w.scope, w.project+"#62", l1.KindIssue, 62, "HIDDEN NEW ACTIVITY", private)
	_, after := walk(t, w, sam, docsArgs)
	if before != after || strings.Contains(after, hiddenDoc) {
		t.Fatalf("hidden document changed reply: %s / %s", before, after)
	}
}

func TestTraversePrincipalsAreTerminal(t *testing.T) {
	w := newWorld(t)
	// Configured names belong on handles; no principal grants or identity fields do.
	r := repo(w.src)
	r.Principals[0].Name = "Kyle"
	calls, err := api.NewCalls(w.pool, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.server = httptest.NewServer(api.Handler(calls, w.pool))
	t.Cleanup(w.server.Close)
	if _, err := w.pool.Exec(t.Context(), `UPDATE l1_docs SET participants='[{"principal_id":"kyle","role":"author"}]' WHERE id=$1`, w.issue); err != nil {
		t.Fatal(err)
	}
	var h api.History
	json.Unmarshal(w.http(t, kyle, "stance_history", map[string]any{"topic": w.lockTopic()}), &h)
	for _, tt := range []struct{ typ, id, edge string }{{"entity", "code:" + w.project, "owners"}, {"stance", h.Current, "author"}, {"document", w.issue, "participants"}} {
		args := traverseArgs(tt.typ, tt.id, map[string]any{"edge": tt.edge})
		g, body := walk(t, w, kyle, args)
		if len(g.Nodes) != 2 || g.Nodes[1].Name != "Kyle" || g.Nodes[1].Type != "principal" {
			t.Fatalf("principal: %s", body)
		}
		var raw struct {
			Nodes []map[string]any `json:"nodes"`
		}
		json.Unmarshal([]byte(body), &raw)
		if len(raw.Nodes[1]) != 4 {
			t.Fatalf("principal carries extra fields: %v", raw.Nodes[1])
		}
		args["steps"] = []map[string]any{{"edge": tt.edge}, {"edge": "documents"}}
		a, b := w.both(t, kyle, "traverse", args)
		if a.status != 400 || a.body != b.body || !b.isError {
			t.Fatalf("principal step=%+v %+v", a, b)
		}
	}
}

func TestTraverseReachByAgentClass(t *testing.T) {
	w := newReachWorld(t)
	pool := newPool(t)
	if err := l2.New(pool).PutEntity(t.Context(), l2.Entity{ID: w.item, Type: l2.TypeTrackerItem, Origin: l2.OriginReference}); err != nil {
		t.Fatal(err)
	}
	principals := []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.AllScopes()}, TokenEnv: "HEARSAY_TRAVERSE_KYLE", Identities: []principal.Identity{{Source: w.src, NativeID: kyleNode}}},
		{ID: "peek", Kind: principal.KindAgent, Class: principal.ClassObserver, Grant: principal.Grant{Scopes: principal.SomeScopes(w.item)}, TokenEnv: "HEARSAY_TRAVERSE_PEEK"},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, Grant: principal.Grant{Scopes: principal.SomeScopes(w.item)}, TokenEnv: "HEARSAY_TRAVERSE_SHED"},
		{ID: "boss", Kind: principal.KindAgent, Class: principal.ClassOrchestrator, Grant: principal.Grant{Scopes: principal.SomeScopes(w.item, w.web)}, TokenEnv: "HEARSAY_TRAVERSE_BOSS"},
	}
	for _, p := range principals {
		t.Setenv(p.TokenEnv, "reach-"+p.ID+"-credential")
	}
	calls, err := api.NewCalls(pool, config.Repo{Principals: principals}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.server = httptest.NewServer(api.Handler(calls, pool))
	t.Cleanup(w.server.Close)
	for _, tt := range []struct {
		caller api.Caller
		want   []string
	}{
		{peekKyle, []string{w.item}},
		{shedForKyle, []string{w.item, w.engine}},
		{bossKyle, []string{w.item, w.engine, w.web}},
	} {
		t.Run(tt.caller.Agent, func(t *testing.T) {
			args := traverseArgs("document", w.pr, map[string]any{"edge": "scope"})
			a, b := w.both(t, tt.caller, "traverse", args)
			if a.status != 200 || b.isError || a.body != b.body {
				t.Fatalf("reach=%+v %+v", a, b)
			}
			var g l3.Subgraph
			json.Unmarshal([]byte(a.body), &g)
			for _, n := range g.Nodes[1:] {
				if !slices.Contains(tt.want, n.ID) {
					t.Fatalf("out of reach node: %+v", n)
				}
			}
			for _, id := range []string{w.item, w.engine, w.repo, w.web} {
				args := traverseArgs("entity", id, map[string]any{"edge": "documents"})
				a, b := w.both(t, tt.caller, "traverse", args)
				if (a.status == 200) != slices.Contains(tt.want, id) || a.body != b.body {
					t.Fatalf("%s: %+v %+v", id, a, b)
				}
			}
		})
	}
}

func TestTraverseLimitsAndRawCandidateCap(t *testing.T) {
	w := newWorld(t)
	traverseEntity(t, w, w.scope)
	args := traverseArgs("entity", w.scope, map[string]any{"edge": "has_part"})
	if _, err := w.pool.Exec(t.Context(), `INSERT INTO l2_entities (id,type,name,part_of,origin) SELECT $1 || ':' || lpad(i::text,4,'0'),'module','child',ARRAY[$2],'config' FROM generate_series(1,201) i`, w.src, w.scope); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, 1, 50, 200} {
		if limit > 0 {
			args["limit"] = limit
		} else {
			delete(args, "limit")
		}
		g, body := walk(t, w, kyle, args)
		want := limit
		if want == 0 {
			want = 50
		}
		if !g.Truncated || len(g.Nodes) != want+1 {
			t.Fatalf("limit %d=%s", limit, body)
		}
		_, repeat := walk(t, w, kyle, args)
		if repeat != body {
			t.Fatal("limit repeat differs")
		}
	}
	// Hidden rows come first by activity, exhausting the raw cap before the
	// readable documents. Removing those rows changes only truncation/missing nodes.
	args = traverseArgs("entity", w.scope, map[string]any{"edge": "documents"})
	absent, absentBytes := walk(t, w, sam, args)
	if absent.Truncated {
		t.Fatal("fixture already truncated")
	}
	if _, err := w.pool.Exec(t.Context(), `INSERT INTO l1_docs (id,kind,artifact_class,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,scope,acl,text,raw_text,body,outcome_kind) SELECT 'l1:' || $1 || ':hidden-' || i,'issue','issue',$1,'hidden-' || i,ARRAY['evt:hidden'],now(),now(),now(),ARRAY[$2],'[{"kind":"identity","source":"secret","native_id":"hidden"}]'::jsonb,'HIDDEN TEXT','HIDDEN TEXT','{"summary":"HIDDEN SUMMARY","outcome_kind":"none"}'::jsonb,'none' FROM generate_series(1,5000) i`, w.src, w.scope); err != nil {
		t.Fatal(err)
	}
	capped, body := walk(t, w, sam, args)
	if !capped.Truncated || len(capped.Nodes) != 1 || len(capped.Edges) != 0 || strings.Contains(body, "hidden-") || strings.Contains(body, "HIDDEN") || strings.Contains(body, "secret") {
		t.Fatalf("raw cap=%s", body)
	}
	if capped.Nodes[0] != absent.Nodes[0] {
		t.Fatalf("cap changed visible start: %+v vs %+v", capped.Nodes[0], absent.Nodes[0])
	}
	_, repeat := walk(t, w, sam, args)
	if repeat != body {
		t.Fatal("cap repeat differs")
	}
	if _, err := w.pool.Exec(t.Context(), `DELETE FROM l1_docs WHERE source=$1 AND source_native_id LIKE 'hidden-%'`, w.src); err != nil {
		t.Fatal(err)
	}
	_, restored := walk(t, w, sam, args)
	if restored != absentBytes {
		t.Fatal("removing hidden rows did not restore byte identity")
	}
}

func TestTraverseAuditContainsOnlyHandleIDs(t *testing.T) {
	w := newWorld(t)
	traverseEntity(t, w, w.scope)
	session := "traverse-session"
	source := w.src + "session"
	author := connector.Identity{Source: source, Kind: connector.IdentityAgent, NativeID: "shed"}
	_, err := l0.New(w.pool).Append(t.Context(), connector.Event{Source: source, NativeID: session + "@start", Kind: connector.KindAgentSession, Time: day, Payload: connector.Payload{Artifact: session, Revision: &connector.Revision{Token: "start", EditedAt: day}, Container: connector.Container{Kind: connector.ContainerKind("stream"), NativeID: "shed"}, Author: &author, Participants: []connector.Participant{{Identity: connector.Identity{Source: source, Kind: connector.IdentityUser, NativeID: "kyle"}, Role: connector.RoleAuthor}}, Native: json.RawMessage(`{"session":"traverse-session"}`)}, ACL: connector.ACL{{Kind: connector.ACLPublic}}})
	if err != nil {
		t.Fatal(err)
	}
	caller := api.Caller{Principal: "kyle", Agent: "shed", Session: session}
	args := traverseArgs("entity", w.scope, map[string]any{"edge": "documents", "where": map[string]any{"class": "merged_pr"}})
	g, _ := walk(t, w, caller, args)
	failed := traverseArgs("entity", "missing", map[string]any{"edge": "documents", "where": map[string]any{"class": "merged_pr"}})
	a, b := w.both(t, caller, "traverse", failed)
	if a.status != 404 || a.body != b.body {
		t.Fatalf("failed=%+v %+v", a, b)
	}
	invalidArgs := traverseArgs("entity", w.scope, map[string]any{"edge": "documents", "where": map[string]any{"state": "SECRET INVALID STATE"}})
	a, b = w.both(t, caller, "traverse", invalidArgs)
	if a.status != 400 || a.body != b.body {
		t.Fatalf("invalid audit call=%+v %+v", a, b)
	}

	rows, err := w.pool.Query(t.Context(), `SELECT payload->'native' FROM l0_events WHERE source='hearsay' AND payload->'native'->>'session'=$1 AND payload->'native'->>'session_source'=$2 ORDER BY seq`, session, source)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	successes, failures := 0, 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var rec api.AuditRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "merged_pr") || strings.Contains(string(raw), "where") || strings.Contains(string(raw), "SECRET INVALID STATE") {
			t.Fatalf("audit has filter: %s", raw)
		}
		if rec.Status == 0 {
			successes++
			if !slices.Equal(rec.TargetIDs, []string{w.scope}) || !slices.Equal(rec.ReturnedIDs, nodeIDs(g)[1:]) {
				t.Fatalf("audit=%s", raw)
			}
		} else {
			failures++
			if (rec.Status != 404 && rec.Status != 400) || len(rec.TargetIDs) > 0 || len(rec.ReturnedIDs) > 0 || rec.Scope != "" {
				t.Fatalf("failed audit=%s", raw)
			}
		}
	}
	if rows.Err() != nil || successes != 2 || failures != 4 {
		t.Fatalf("audits successes=%d failures=%d err=%v", successes, failures, rows.Err())
	}
}

func TestTraverseFiltersOrderingAndStoredDirections(t *testing.T) {
	w := newWorld(t)
	traverseEntity(t, w, w.scope)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	docs := []string{}
	for _, key := range []string{"z", "a", "b"} {
		id := w.putIn(w.scope, w.project+"#order-"+key, l1.KindIssue, 80, "first line\nPRIVATE FULL SECOND LINE", public)
		docs = append(docs, id)
		if _, err := w.pool.Exec(t.Context(), `UPDATE l1_docs SET state='started' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(docs)
	tp := traverseTopic(t, w, docs[0], "ordered topic", w.scope)
	old := traverseStance(t, w, tp, docs[0], "old", 80)
	middle := traverseStance(t, w, tp, docs[1], "middle", 81)
	newest := traverseStance(t, w, tp, docs[2], "newest", 82)
	since := day.Add(80 * time.Hour).Format(time.RFC3339)
	for _, tt := range []struct {
		name   string
		args   map[string]any
		want   []string
		stored string
	}{
		{"documents", traverseArgs("entity", w.scope, map[string]any{"edge": "documents", "where": map[string]any{"kind": "issue", "class": "issue", "state": "started", "since": since}}), docs, "documents"},
		{"current", traverseArgs("topic", tp.ID, map[string]any{"edge": "stances", "where": map[string]any{"current": true}}), []string{newest.ID}, "stances"},
		{"historical since", traverseArgs("topic", tp.ID, map[string]any{"edge": "stances", "where": map[string]any{"current": false, "since": day.Add(81 * time.Hour).Format(time.RFC3339)}}), []string{middle.ID}, "stances"},
		{"stances order", traverseArgs("topic", tp.ID, map[string]any{"edge": "stances"}), []string{newest.ID, middle.ID, old.ID}, "stances"},
		{"evidence", traverseArgs("stance", old.ID, map[string]any{"edge": "evidence"}), []string{docs[0]}, "evidence"},
		{"citation inverse", traverseArgs("document", docs[0], map[string]any{"edge": "cited_by"}), []string{old.ID}, "evidence"},
		{"about inverse", traverseArgs("topic", tp.ID, map[string]any{"edge": "about"}), []string{w.scope}, "topics"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, body := walk(t, w, sam, tt.args)
			if !slices.Equal(nodeIDs(g)[1:], tt.want) {
				t.Fatalf("got %s, want %v", body, tt.want)
			}
			if strings.Contains(body, "PRIVATE FULL SECOND LINE") {
				t.Fatalf("more than one line: %s", body)
			}
			for _, e := range g.Edges {
				if e.Edge != tt.stored {
					t.Fatalf("edge=%+v", e)
				}
			}
			_, again := walk(t, w, sam, tt.args)
			if again != body {
				t.Fatal("repeat differs")
			}
		})
	}
	// Topic ordering uses readable history only, with newest-first and id ties.
	a := traverseTopic(t, w, docs[1], "newest topic", w.scope)
	traverseStance(t, w, a, docs[1], "position", 84)
	b := traverseTopic(t, w, docs[2], "tied newest topic", w.scope)
	traverseStance(t, w, b, docs[2], "position", 84)
	want := []string{a.ID, b.ID}
	slices.Sort(want)
	want = append(want, tp.ID)
	g, body := walk(t, w, sam, traverseArgs("entity", w.scope, map[string]any{"edge": "topics", "where": map[string]any{"since": since}}))
	if !slices.Equal(nodeIDs(g)[1:], want) {
		t.Fatalf("topics order=%s, want %v", body, want)
	}
	// A readable opener can have an unreadable current stance. Such a topic has
	// no current position or tier and cannot match a tier filter.
	hidden := w.putIn(w.scope, w.project+"#hidden-current", l1.KindPR, 90, "HIDDEN CURRENT", connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}})
	traverseStance(t, w, tp, hidden, "HIDDEN POSITION", 90)
	g, body = walk(t, w, sam, traverseArgs("topic", tp.ID, map[string]any{"edge": "about"}))
	if g.Nodes[0].Tier != "" || g.Nodes[0].Current != nil || strings.Contains(body, "HIDDEN") {
		t.Fatalf("hidden current=%s", body)
	}
	g, body = walk(t, w, sam, traverseArgs("entity", w.scope, map[string]any{"edge": "topics", "where": map[string]any{"tier": "inferred", "since": since}}))
	if slices.Contains(nodeIDs(g), tp.ID) {
		t.Fatalf("tier filter matched unreadable current: %s", body)
	}
}

// All document-target edges must carry activity into both the per-source cut
// and the frontier-wide ordering, including edges that start with only IDs.
func TestTraverseDocumentCandidateOrdering(t *testing.T) {
	w := newWorld(t)
	entity := "code:" + w.src + "-document-order"
	traverseEntity(t, w, entity)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	old := w.putIn(entity, w.project+"#a-old", l1.KindIssue, 80, "old", public)
	tieA := w.putIn(entity, w.project+"#b-tied", l1.KindIssue, 81, "tied a", public)
	tieB := w.putIn(entity, w.project+"#y-tied", l1.KindIssue, 81, "tied b", public)
	newest := w.putIn(entity, w.project+"#z-newest", l1.KindIssue, 82, "newest", public)
	docs := []string{old, tieA, tieB, newest}
	for i, doc := range docs {
		if _, err := l2.New(w.pool).Pin(t.Context(), l2.Pin{Scope: entity, L1: doc, PinnedBy: "kyle", PinnedAt: day.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	stance := func(name string, evidence []string) string {
		tp := traverseTopic(t, w, evidence[0], name, entity)
		at := day.Add(83 * time.Hour)
		st, _, err := l2.New(w.pool).AppendStance(t.Context(), l2.Stance{
			ID: l2.StanceID(tp.ID, evidence[0], name, at, l2.TierInferred), TopicID: tp.ID,
			Position: name, Author: "kyle", StatedAt: at, Evidence: evidence, Tier: l2.TierInferred, ACL: public,
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		return st.ID
	}
	all := stance("all evidence", docs)
	older := stance("older evidence", []string{old, tieB})
	newer := stance("newer evidence", []string{newest, tieA})
	multi := traverseArgs("stance", older, map[string]any{"edge": "evidence"})
	multi["start"] = []map[string]string{{"type": "stance", "id": older}, {"type": "stance", "id": newer}}
	for _, tt := range []struct {
		name   string
		args   map[string]any
		starts int
	}{
		{"evidence", traverseArgs("stance", all, map[string]any{"edge": "evidence"}), 1},
		{"evidence across frontier", multi, 2},
		{"documents", traverseArgs("entity", entity, map[string]any{"edge": "documents"}), 1},
		{"anchors", traverseArgs("entity", entity, map[string]any{"edge": "anchors"}), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, limit := range []int{50, 1} {
				t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
					tt.args["limit"] = limit
					g, body := walk(t, w, sam, tt.args)
					want := []string{newest, tieA, tieB, old}
					if limit == 1 {
						want = want[:1]
					}
					if !slices.Equal(nodeIDs(g)[tt.starts:], want) || g.Truncated != (limit == 1) {
						t.Fatalf("document order=%s, want %v, truncated=%v", body, want, limit == 1)
					}
					_, again := walk(t, w, sam, tt.args)
					if again != body {
						t.Fatal("document order repeat differs")
					}
				})
			}
		})
	}
}

func TestTraverseAnchorRawCandidateCap(t *testing.T) {
	for _, tt := range []struct {
		name          string
		precedingStep bool
	}{
		{"direct", false},
		{"remaining budget after has_part", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			entity := "code:" + w.src + "-anchor-cap"
			parent := "code:" + w.src + "-anchor-parent"
			traverseEntity(t, w, parent)
			traverseEntity(t, w, entity, parent)
			doc := w.putIn(entity, w.project+"#readable-anchor", l1.KindIssue, 80, "readable anchor", connector.ACL{{Kind: connector.ACLPublic}})
			if _, err := l2.New(w.pool).Pin(t.Context(), l2.Pin{Scope: entity, L1: doc, PinnedBy: "kyle", PinnedAt: day.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			args := traverseArgs("entity", entity, map[string]any{"edge": "anchors"})
			hiddenCount := l3.CandidateLimit
			if tt.precedingStep {
				args = traverseArgs("entity", parent, map[string]any{"edge": "has_part"}, map[string]any{"edge": "anchors"})
				hiddenCount--
			}
			absent, absentBytes := walk(t, w, sam, args)
			if absent.Truncated || !slices.Contains(nodeIDs(absent), doc) {
				t.Fatalf("hidden-absent fixture=%s", absentBytes)
			}
			// Pin ordering is independent of activity: every hidden pin is older
			// than the readable pin, even though its document is newer.
			if _, err := w.pool.Exec(t.Context(), `INSERT INTO l1_docs (id,kind,artifact_class,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,scope,acl,text,raw_text,body,outcome_kind)
SELECT 'l1:' || $1 || ':hidden-pin-' || i,'issue','issue',$1,'hidden-pin-' || i,ARRAY['evt:hidden-pin'],now(),now(),now(),ARRAY[$2],'[{"kind":"identity","source":"secret","native_id":"hidden"}]'::jsonb,'HIDDEN TEXT','HIDDEN TEXT','{"summary":"HIDDEN SUMMARY","outcome_kind":"none"}'::jsonb,'none' FROM generate_series(1,$3::int) i`, w.src, entity, hiddenCount); err != nil {
				t.Fatal(err)
			}
			if _, err := w.pool.Exec(t.Context(), `INSERT INTO l2_pins (scope,l1,pinned_by,pinned_at) SELECT $1,id,'HIDDEN PINNER',$3 FROM l1_docs WHERE source=$2 AND source_native_id LIKE 'hidden-pin-%'`, entity, w.src, day); err != nil {
				t.Fatal(err)
			}
			capped, body := walk(t, w, sam, args)
			// Only the later readable anchor and its edge can disappear.
			want := absent
			want.Nodes = slices.DeleteFunc(slices.Clone(absent.Nodes), func(n l3.Node) bool { return n.ID == doc })
			want.Edges = slices.DeleteFunc(slices.Clone(absent.Edges), func(e l3.Edge) bool { return e.To.ID == doc })
			want.Truncated = true
			if string(mustJSON(t, capped)) != string(mustJSON(t, want)) {
				t.Fatalf("anchor cap=%s, want %+v", body, want)
			}
			for _, hidden := range []string{"hidden-pin", "HIDDEN", "secret"} {
				if strings.Contains(body, hidden) {
					t.Fatalf("hidden anchor data in %s", body)
				}
			}
			_, again := walk(t, w, sam, args)
			if again != body {
				t.Fatal("anchor cap repeat differs")
			}
			// The bundle view remains unbounded by traversal's raw-row cap.
			var bundle struct {
				Anchors []struct {
					L1 string `json:"l1"`
				} `json:"anchors"`
			}
			json.Unmarshal(w.http(t, sam, "get_bundle", map[string]any{"scope": entity}), &bundle)
			if len(bundle.Anchors) != 1 || bundle.Anchors[0].L1 != doc {
				t.Fatalf("bundle lost readable anchor: %+v", bundle.Anchors)
			}
			// Below the cap, hidden pins have no effect on any returned byte.
			if _, err := w.pool.Exec(t.Context(), `DELETE FROM l2_pins WHERE scope=$1 AND l1 LIKE '%:hidden-pin-%' AND l1 <> $2`, entity, "l1:"+w.src+":hidden-pin-1"); err != nil {
				t.Fatal(err)
			}
			_, belowCap := walk(t, w, sam, args)
			if belowCap != absentBytes {
				t.Fatal("below-cap hidden pins changed reply")
			}
		})
	}
}

// Missing pins and duplicate fallback results still consume raw rows. In
// particular, an inferred spec also read by the defaults must be charged twice.
func TestTraverseAnchorFallbackBudget(t *testing.T) {
	for _, inferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("inferred %v", inferred), func(t *testing.T) {
			w := newWorld(t)
			entity := "code:" + w.src + "-anchor-fallback"
			traverseEntity(t, w, entity)
			public := connector.ACL{{Kind: connector.ACLPublic}}
			older := w.putIn(entity, w.project+"#older-spec", l1.KindIssue, 79, "older spec", public)
			artifact := w.project + "#newer-spec"
			newer := w.putIn(entity, artifact, l1.KindIssue, 80, "newer spec", public)
			if _, err := w.pool.Exec(t.Context(), `UPDATE l1_docs SET artifact_class='spec' WHERE id=ANY($1::text[])`, []string{older, newer}); err != nil {
				t.Fatal(err)
			}
			missing := l3.CandidateLimit - 1
			if inferred {
				referrer := w.putIn(entity, w.project+"#referrer", l1.KindIssue, 1, "referrer", public)
				refs := mustJSON(t, []l1.Reference{{Type: l1.RefTrackerItem, ID: artifact}})
				if _, err := w.pool.Exec(t.Context(), `UPDATE l1_docs SET refs=$1 WHERE id=$2`, refs, referrer); err != nil {
					t.Fatal(err)
				}
				missing-- // Inference takes one slot before the duplicate default.
			}
			args := traverseArgs("entity", entity, map[string]any{"edge": "anchors"})
			absent, absentBytes := walk(t, w, sam, args)
			if absent.Truncated || !slices.Equal(nodeIDs(absent), []string{entity, newer, older}) {
				t.Fatalf("fallback fixture=%s", absentBytes)
			}
			if _, err := w.pool.Exec(t.Context(), `INSERT INTO l2_pins (scope,l1,pinned_by,pinned_at) SELECT $1,'l1:' || $2 || ':missing-pin-' || i,'UNREADABLE PINNER',$3 FROM generate_series(1,$4::int) i`, entity, w.src, day, missing); err != nil {
				t.Fatal(err)
			}
			capped, body := walk(t, w, sam, args)
			if !capped.Truncated || !slices.Equal(nodeIDs(capped), []string{entity, newer}) || len(capped.Edges) != 1 || strings.Contains(body, "missing-pin") || strings.Contains(body, "UNREADABLE") {
				t.Fatalf("fallback budget=%s", body)
			}
			_, again := walk(t, w, sam, args)
			if again != body {
				t.Fatal("fallback cap repeat differs")
			}
		})
	}
}

func TestTraverseDepthRangesCyclesAndLowestStep(t *testing.T) {
	w := newWorld(t)
	root := "code:" + w.src + "-depth"
	a, b, c := root+":a", root+":b", root+":c"
	for _, item := range []struct {
		id     string
		parent []string
	}{{root, nil}, {a, []string{root}}, {b, []string{a}}, {c, []string{b}}} {
		traverseEntity(t, w, item.id, item.parent...)
	}
	for _, tt := range []struct {
		depth string
		want  []string
	}{{"1", []string{a}}, {"1..3", []string{a, b, c}}, {"2..3", []string{b, c}}, {"3", []string{c}}} {
		t.Run(tt.depth, func(t *testing.T) {
			g, body := walk(t, w, kyle, traverseArgs("entity", root, map[string]any{"edge": "has_part", "depth": tt.depth}))
			if !slices.Equal(nodeIDs(g)[1:], tt.want) {
				t.Fatalf("depth=%s", body)
			}
		})
	}
	// Reaching a start again does not change its step; cycles terminate.
	traverseEntity(t, w, root, c)
	args := traverseArgs("entity", root, map[string]any{"edge": "has_part", "depth": "1..6"}, map[string]any{"edge": "part_of", "depth": "1..6"})
	args["start"] = []map[string]string{{"type": "entity", "id": root}, {"type": "entity", "id": b}}
	g, body := walk(t, w, kyle, args)
	if len(g.Nodes) != 4 || g.Truncated || g.Nodes[0].Step != 0 || g.Nodes[1].Step != 0 {
		t.Fatalf("cycle=%s", body)
	}
	for _, n := range g.Nodes[2:] {
		if n.Step != 1 {
			t.Fatalf("lowest step=%s", body)
		}
	}
}

func TestTraverseRawTopicRowsCountBeforeLedgerDeduplication(t *testing.T) {
	w := newWorld(t)
	// Use a separate entity so exactly 5,000 raw topic rows are candidates.
	entity := "code:" + w.src + "-merged-cap"
	traverseEntity(t, w, entity)
	target := "topic:" + w.src + ":target"
	_, err := w.pool.Exec(t.Context(), `INSERT INTO l2_topics (id,scope,name,about,acl,opened_by) SELECT 'topic:' || $1 || ':' || i,$1,'merged source',ARRAY[$2],'[{"kind":"public"}]'::jsonb,$3 FROM generate_series(1,4999) i`, w.src, entity, w.issue)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.pool.Exec(t.Context(), `INSERT INTO l2_topics (id,scope,name,about,acl,opened_by) VALUES ($1,$2,'merge target',ARRAY[$3],'[{"kind":"public"}]'::jsonb,$4)`, target, w.src, entity, w.issue)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.pool.Exec(t.Context(), `INSERT INTO l2_topic_operations (kind,scope,principal,topics,stances) SELECT 'merge',$1,'kyle',ARRAY[$2,'topic:' || $1 || ':' || i],'{}'::text[] FROM generate_series(1,4999) i`, w.src, target)
	if err != nil {
		t.Fatal(err)
	}
	args := traverseArgs("entity", entity, map[string]any{"edge": "topics"})
	g, body := walk(t, w, kyle, args)
	if !g.Truncated || !slices.Equal(nodeIDs(g), []string{entity, target}) {
		t.Fatalf("raw topic cap=%s", body)
	}
	_, repeat := walk(t, w, kyle, args)
	if repeat != body {
		t.Fatal("merged cap repeat differs")
	}
}

func TestTraverseHiddenStandingMatchesHistory(t *testing.T) {
	for _, fixture := range []string{"hidden current", "hidden supersession ancestor"} {
		t.Run(fixture, func(t *testing.T) {
			w := newWorld(t)
			entity := "code:" + w.src + "-standing"
			traverseEntity(t, w, entity)
			public := connector.ACL{{Kind: connector.ACLPublic}}
			private := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}}
			doc := w.putIn(entity, w.project+"#standing-public", l1.KindIssue, 100, "readable evidence", public)
			tp := traverseTopic(t, w, doc, "readable topic", entity)
			var hidden l2.Stance
			var readable []l2.Stance
			presentTier := ""
			if fixture == "hidden current" {
				readable = []l2.Stance{traverseStance(t, w, tp, doc, "readable position", 100)}
				secret := w.putIn(entity, w.project+"#standing-secret", l1.KindPR, 101, "SECRET EVIDENCE CONTENT", private)
				hidden = traverseStance(t, w, tp, secret, "SECRET POSITION", 101)
			} else {
				// Same fork as "a hidden stance hides only itself from the
				// contest" in l2's TestStand: late changes a, b restates a.
				secret := w.putIn(entity, w.project+"#standing-secret", l1.KindIssue, 99, "SECRET EVIDENCE CONTENT", private)
				hidden = traverseStance(t, w, tp, secret, "SECRET POSITION", 99)
				late := traverseStance(t, w, tp, doc, "readable change", 100)
				second := w.putIn(entity, w.project+"#standing-second", l1.KindIssue, 101, "readable second evidence", public)
				b := traverseStance(t, w, tp, second, "readable restatement", 101)
				if _, err := w.pool.Exec(t.Context(), `UPDATE l2_stances SET supersedes=$1, judgement=$2 WHERE id=$3`, hidden.ID, l2.JudgementChanges, late.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := w.pool.Exec(t.Context(), `UPDATE l2_stances SET supersedes=$1, judgement=$2 WHERE id=$3`, hidden.ID, l2.JudgementRestates, b.ID); err != nil {
					t.Fatal(err)
				}
				readable = []l2.Stance{b, late}
				presentTier = "contested"
			}
			if _, err := w.pool.Exec(t.Context(), `UPDATE l2_stances SET author='SECRET AUTHOR' WHERE id=$1`, hidden.ID); err != nil {
				t.Fatal(err)
			}
			checkSecrets := func(body string) {
				t.Helper()
				for _, secret := range append([]string{hidden.ID, "SECRET POSITION", "SECRET AUTHOR", "SECRET EVIDENCE CONTENT", "standing-secret"}, hidden.Evidence...) {
					if strings.Contains(body, secret) {
						t.Fatalf("hidden data %q in reply: %s", secret, body)
					}
				}
			}
			cases := []struct {
				name string
				args map[string]any
			}{
				{"all stances", traverseArgs("topic", tp.ID, map[string]any{"edge": "stances"})},
				{"current stances", traverseArgs("topic", tp.ID, map[string]any{"edge": "stances", "where": map[string]any{"current": true}})},
				{"historical stances", traverseArgs("topic", tp.ID, map[string]any{"edge": "stances", "where": map[string]any{"current": false}})},
				{"tier then stances", traverseArgs("entity", entity, map[string]any{"edge": "topics", "where": map[string]any{"tier": "inferred"}}, map[string]any{"edge": "stances"})},
				{"tier with limit then stances", traverseArgs("entity", entity, map[string]any{"edge": "topics", "where": map[string]any{"tier": "inferred"}}, map[string]any{"edge": "stances"})},
			}
			cases[4].args["limit"] = 1
			present := map[string]l3.Subgraph{}
			for _, phase := range []string{"present", "absent"} {
				if phase == "absent" {
					// Remove incoming foreign keys with the absent ancestor.
					if _, err := w.pool.Exec(t.Context(), `UPDATE l2_stances SET supersedes=NULL WHERE supersedes=$1`, hidden.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := w.pool.Exec(t.Context(), `DELETE FROM l2_stances WHERE id=$1`, hidden.ID); err != nil {
						t.Fatal(err)
					}
				}
				h, m := w.both(t, sam, "stance_history", map[string]any{"topic": tp.ID})
				if h.status != 200 || m.isError || h.body != m.body {
					t.Fatalf("history HTTP=%+v MCP=%+v", h, m)
				}
				var history api.History
				if err := json.Unmarshal([]byte(h.body), &history); err != nil {
					t.Fatal(err)
				}
				wantTier, wantCurrent := presentTier, ""
				if phase == "absent" {
					wantTier = "inferred"
				}
				if wantTier != "" {
					wantCurrent = readable[0].ID
				}
				if history.Tier != wantTier || history.Current != wantCurrent {
					t.Fatalf("%s history=%+v, want tier=%s current=%s", phase, history, wantTier, wantCurrent)
				}
				for _, tt := range cases {
					t.Run(phase+"/"+tt.name, func(t *testing.T) {
						g, body := walk(t, w, sam, tt.args)
						checkSecrets(body)
						_, repeat := walk(t, w, sam, tt.args)
						if repeat != body {
							t.Fatal("standing repeat differs")
						}
						for _, n := range g.Nodes {
							if n.Type == "topic" {
								var position any
								if wantCurrent != "" {
									position = readable[0].Position
								}
								if n.Tier != history.Tier || n.Current != position {
									t.Fatalf("topic standing differs from history: %s / %+v", body, history)
								}
							}
							if n.Type == "stance" && n.Current != (n.ID == history.Current) {
								t.Fatalf("stance current differs from history: %s / %+v", body, history)
							}
						}
						want := []string{tp.ID}
						switch tt.name {
						case "all stances", "historical stances":
							for _, st := range readable {
								if tt.name == "all stances" || st.ID != wantCurrent {
									want = append(want, st.ID)
								}
							}
						case "current stances":
							if wantCurrent != "" {
								want = append(want, wantCurrent)
							}
						default:
							want = []string{entity}
							if phase == "absent" {
								want = append(want, tp.ID)
								if tt.name == "tier then stances" {
									for _, st := range readable {
										want = append(want, st.ID)
									}
								}
							}
						}
						if !slices.Equal(nodeIDs(g), want) || g.Truncated != (phase == "absent" && tt.name == "tier with limit then stances") || len(g.Edges) != len(want)-1 {
							t.Fatalf("standing filter result=%s, want ids=%v", body, want)
						}
						if phase == "present" {
							present[tt.name] = g
						} else if tt.name == "all stances" {
							// With no standing filter, only standing fields may differ.
							normalize := func(graph l3.Subgraph) string {
								for i := range graph.Nodes {
									graph.Nodes[i].Current = nil
									graph.Nodes[i].Tier = ""
								}
								return string(mustJSON(t, graph))
							}
							if normalize(g) != normalize(present[tt.name]) {
								t.Fatalf("hidden standing changed other fields: %s", body)
							}
						}
					})
				}
			}
		})
	}
}
