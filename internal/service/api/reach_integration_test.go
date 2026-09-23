//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

// reachWorld is a repository with two directories, a pull request that
// touched one of them, and agents of every class scoped to that pull request's
// tracker item (#148).
//
//	code:P            the repository; the readme is about it alone
//	├─ code:P:engine  the pull request touched it; the engine doc is about it
//	└─ code:P:web     the web doc is about it
//	tracker:…#20      the pull request itself
type reachWorld struct {
	src, project, item     string
	repo, engine, web      string
	pr, engineDoc, webDoc  string
	readme, orphan, secret string
	undistilled            string
	ownTopic, engineTopic  string
	repoTopic              string
	server                 *httptest.Server
	events                 *l0.Store
}

// The principals: kyle reaches everything; ann is granted the pull request and
// the web directory. Every agent is granted the pull request's item, and pry
// the engine as well.
var (
	annAlone    = api.Caller{Principal: "ann"}
	shedForKyle = api.Caller{Principal: "kyle", Agent: "shed"}
	pryKyle     = api.Caller{Principal: "kyle", Agent: "pry"}
	pryAnn      = api.Caller{Principal: "ann", Agent: "pry"}
	bossKyle    = api.Caller{Principal: "kyle", Agent: "boss"}
	bossAnn     = api.Caller{Principal: "ann", Agent: "boss"}
)

func newReachWorld(t *testing.T) *reachWorld {
	t.Helper()
	pool := newPool(t)
	ctx := t.Context()
	w := &reachWorld{src: newSource(), events: l0.New(pool)}
	w.project = "acme/" + w.src
	w.repo, w.engine, w.web = "code:"+w.project, "code:"+w.project+":engine", "code:"+w.project+":web"
	w.item = "tracker:" + w.src + ":" + w.project + "#20"
	docs, graph := l1.New(pool), l2.New(pool)

	public := connector.ACL{{Kind: connector.ACLPublic}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}}
	put := func(artifact string, kind l1.Kind, hour int, text string, acl connector.ACL, scope []string, refs ...l1.Reference) string {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		ev := connector.Event{
			Source: w.src, NativeID: artifact, Kind: connector.KindIssue, Time: at,
			Payload: connector.Payload{
				Artifact: artifact, Title: text,
				Container: connector.Container{Kind: connector.ContainerRepository, NativeID: w.project},
				Author:    &connector.Identity{Source: w.src, Kind: connector.IdentityUser, NativeID: kyleNode},
			},
			ACL: acl,
		}
		class := config.ArtifactIssue
		if kind == l1.KindPR {
			ev.Kind, class = connector.KindPullRequest, config.ArtifactMergedPR
		}
		if _, err := w.events.Append(ctx, ev); err != nil {
			t.Fatalf("Append(%s) = %v", artifact, err)
		}
		doc := l1.Document{
			ID: l1.DocID(w.src, artifact), Kind: kind, ArtifactClass: class,
			Source: l1.Source{System: w.src, NativeID: artifact},
			L0Refs: []string{connector.EventID(w.src, artifact)},
			Time:   l1.Times{Created: at, Updated: at, LastActivity: at},
			Scope:  scope, References: refs,
			ACL: acl, Text: text, RawText: text,
			Body: l1.Body{Summary: text, OutcomeKind: l1.OutcomeDecided},
		}
		if _, err := docs.Put(ctx, doc); err != nil {
			t.Fatalf("Put(%s) = %v", doc.ID, err)
		}
		return doc.ID
	}
	w.pr = put(w.project+"#20", l1.KindPR, 1, "Moves the lock into the worker.", public,
		[]string{w.repo, w.engine, w.item}, l1.Reference{Type: l1.RefSystem, ID: w.engine})
	w.engineDoc = put(w.project+"#30", l1.KindIssue, 2, "The engine retries every write twice.", public, []string{w.repo, w.engine})
	w.webDoc = put(w.project+"#31", l1.KindIssue, 3, "The web client caches sessions.", public, []string{w.repo, w.web})
	w.readme = put(w.project+"#32", l1.KindIssue, 4, "Every service logs as JSON.", public, []string{w.repo})
	w.orphan = put(w.project+"#33", l1.KindIssue, 5, "A note about nothing in particular.", public, nil)
	w.secret = put(w.project+"#34", l1.KindIssue, 6, "The pull request's private follow-up.", private, []string{w.item})
	// An event nothing was distilled from yet.
	undistilled := connector.Event{
		Source: w.src, NativeID: w.project + "#40", Kind: connector.KindIssue, Time: day,
		Payload: connector.Payload{Artifact: w.project + "#40", Title: "Not distilled yet.",
			Container: connector.Container{Kind: connector.ContainerRepository, NativeID: w.project},
			Author:    &connector.Identity{Source: w.src, Kind: connector.IdentityUser, NativeID: kyleNode}},
		ACL: public,
	}
	if _, err := w.events.Append(ctx, undistilled); err != nil {
		t.Fatal(err)
	}
	w.undistilled = connector.EventID(w.src, w.project+"#40")

	for _, e := range []l2.Entity{
		{ID: w.repo, Type: l2.TypeProject, Name: w.src + "-repo", Origin: l2.OriginConfig},
		{ID: w.engine, Type: l2.TypeModule, Name: w.src + "-engine", PartOf: []string{w.repo}, Origin: l2.OriginConfig},
		{ID: w.web, Type: l2.TypeModule, Name: w.src + "-web", PartOf: []string{w.repo}, Origin: l2.OriginConfig},
	} {
		if err := graph.PutEntity(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	topic := func(doc, name string, about string, position string, hour int) string {
		t.Helper()
		tp := l2.Topic{ID: l2.TopicID(w.src, doc, 0, name), Scope: w.src, Name: name, About: []string{about}, ACL: public, OpenedBy: doc}
		if _, err := graph.OpenTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
		at := day.Add(time.Duration(hour) * time.Hour)
		if _, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(tp.ID, doc, position, at, l2.TierInferred), TopicID: tp.ID, Position: position, Author: "kyle",
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: public,
		}, at); err != nil {
			t.Fatal(err)
		}
		return tp.ID
	}
	w.ownTopic = topic(w.pr, "where the lock lives", w.item, "in the worker", 1)
	w.engineTopic = topic(w.engineDoc, "how the engine retries", w.engine, "twice, then gives up", 2)
	w.repoTopic = topic(w.readme, "how services log", w.repo, "as JSON", 4)

	tokens := map[string]string{}
	principals := []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.AllScopes()},
			Identities: []principal.Identity{{Source: w.src, NativeID: kyleNode}}},
		{ID: "ann", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.SomeScopes(w.item, w.web)},
			Identities: []principal.Identity{{Source: w.src, NativeID: "ann-node"}}},
		{ID: "peek", Kind: principal.KindAgent, Class: principal.ClassObserver, Grant: principal.Grant{Scopes: principal.SomeScopes(w.item)}},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, Grant: principal.Grant{Scopes: principal.SomeScopes(w.item)}},
		{ID: "pry", Kind: principal.KindAgent, Class: principal.ClassObserver, Grant: principal.Grant{Scopes: principal.SomeScopes(w.item, w.engine)}},
		{ID: "boss", Kind: principal.KindAgent, Class: principal.ClassSteward, Grant: principal.Grant{Scopes: principal.SomeScopes(w.item)}},
	}
	for i := range principals {
		env := "HEARSAY_REACH_" + strings.ToUpper(principals[i].ID) + "_TOKEN"
		principals[i].TokenEnv = env
		tokens[principals[i].ID] = "reach-" + principals[i].ID + "-credential"
		t.Setenv(env, tokens[principals[i].ID])
	}
	calls, err := api.NewCalls(pool, config.Repo{Principals: principals}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.server = httptest.NewServer(api.Handler(calls, pool))
	t.Cleanup(w.server.Close)
	return w
}

// post calls over HTTP with the caller's credentials and returns the status and
// the body.
func (w *reachWorld) post(t *testing.T, path string, caller api.Caller, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.PrincipalHeader, caller.Principal)
	req.Header.Set(api.AuthorizationHeader, "Bearer reach-"+caller.Principal+"-credential")
	if caller.Agent != "" {
		req.Header.Set(api.AgentHeader, caller.Agent)
		req.Header.Set(api.AgentTokenHeader, "reach-"+caller.Agent+"-credential")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, got
}

// answer is what one call was served over one interface: the status (HTTP's,
// or 200 and isError over MCP) and the bytes.
type answer struct {
	status  int
	isError bool
	body    string
}

// both makes one call over HTTP and over MCP.
func (w *reachWorld) both(t *testing.T, caller api.Caller, call string, args any) (overHTTP, overMCP answer) {
	t.Helper()
	body, _ := json.Marshal(args)
	status, got := w.post(t, "/v1/"+call, caller, body)
	overHTTP = answer{status: status, body: string(got)}
	rpc, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": call, "arguments": args},
	})
	status, got = w.post(t, "/mcp", caller, rpc)
	var resp struct {
		Result api.ToolResult `json:"result"`
	}
	if err := json.Unmarshal(got, &resp); err != nil || len(resp.Result.Content) != 1 {
		t.Fatalf("tools/call %s answered %d %s", call, status, got)
	}
	overMCP = answer{status: status, isError: resp.Result.IsError, body: resp.Result.Content[0].Text}
	return overHTTP, overMCP
}

// reads reports whether a caller may read a document, over both interfaces,
// and fails the test if the two disagree.
func (w *reachWorld) reads(t *testing.T, caller api.Caller, doc string) bool {
	t.Helper()
	overHTTP, overMCP := w.both(t, caller, "get_l1", map[string]any{"id": doc})
	if overHTTP.body != overMCP.body || (overHTTP.status == http.StatusOK) == overMCP.isError {
		t.Fatalf("get_l1(%s) for %+v: HTTP %+v and MCP %+v disagree", doc, caller, overHTTP, overMCP)
	}
	return overHTTP.status == http.StatusOK
}

// Every class's reach, through get_l1 over both interfaces: an observer reads
// its scope and no linked code, a worker the code its scope links to, a person
// and an agent only what both reach, and a steward everything the person
// reaches whatever its own scopes say — and the access lists hold inside every
// reach.
func TestReachByClass(t *testing.T) {
	w := newReachWorld(t)
	for _, tt := range []struct {
		name   string
		caller api.Caller
		reads  []string
		denied []string
	}{{
		name:   "a person granted everything",
		caller: kyle,
		reads:  []string{w.pr, w.engineDoc, w.webDoc, w.readme, w.orphan, w.secret},
	}, {
		// ann reaches the engine through the pull request her scope holds,
		// and nothing about the repository alone or about no entity.
		name:   "a person with scopes",
		caller: annAlone,
		reads:  []string{w.pr, w.engineDoc, w.webDoc},
		denied: []string{w.readme, w.orphan, w.secret},
	}, {
		name:   "an observer reads its scope and not the code it links to",
		caller: peekKyle,
		reads:  []string{w.pr, w.secret},
		denied: []string{w.engineDoc, w.webDoc, w.readme, w.orphan},
	}, {
		name:   "a worker reads the code its scope links to",
		caller: shedForKyle,
		reads:  []string{w.pr, w.engineDoc, w.secret},
		denied: []string{w.webDoc, w.readme, w.orphan},
	}, {
		name:   "an agent granted the engine reads it for a person who reaches it",
		caller: pryKyle,
		reads:  []string{w.pr, w.engineDoc},
		denied: []string{w.webDoc, w.readme},
	}, {
		// ann reads the web doc and pry, for kyle, the engine doc; pry acting
		// for ann reads neither: the intersection is narrower than both.
		name:   "a person and an agent reach only what both do",
		caller: pryAnn,
		reads:  []string{w.pr},
		denied: []string{w.engineDoc, w.webDoc, w.readme, w.secret},
	}, {
		name:   "a steward reads everything the person does, beyond its own scopes",
		caller: bossKyle,
		reads:  []string{w.pr, w.engineDoc, w.webDoc, w.readme, w.orphan, w.secret},
	}, {
		// Still bound by the person: what ann does not reach, and what her
		// access lists do not allow, a steward acting for her does not read.
		name:   "a steward is bound by the person",
		caller: bossAnn,
		reads:  []string{w.pr, w.engineDoc, w.webDoc},
		denied: []string{w.readme, w.orphan, w.secret},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			for _, doc := range tt.reads {
				if !w.reads(t, tt.caller, doc) {
					t.Errorf("%+v may not read %s, want it read", tt.caller, doc)
				}
			}
			for _, doc := range tt.denied {
				if w.reads(t, tt.caller, doc) {
					t.Errorf("%+v read %s, want it out of reach", tt.caller, doc)
				}
			}
		})
	}
}

// The bundle holds only what is in reach: an observer's has the pull request's
// own stance and no linked entity; a worker's has the engine and its stance as
// well; and neither inherits the repository's stance, whose topic is about an
// ancestor outside both reaches. Each is identical over both interfaces.
func TestABundleHoldsOnlyWhatIsInReach(t *testing.T) {
	w := newReachWorld(t)
	bundleFor := func(caller api.Caller) bundle.Bundle {
		t.Helper()
		overHTTP, overMCP := w.both(t, caller, "get_bundle", map[string]any{"scope": w.item})
		if overHTTP.status != http.StatusOK || overMCP.isError || overHTTP.body != overMCP.body {
			t.Fatalf("get_bundle for %+v: HTTP %+v MCP %+v", caller, overHTTP, overMCP)
		}
		return decodeBundle(t, []byte(overHTTP.body))
	}
	entities := func(b bundle.Bundle) []string {
		var ids []string
		for _, e := range b.Scope.Entities {
			ids = append(ids, e.ID)
		}
		return ids
	}
	topics := func(b bundle.Bundle) []string {
		var ids []string
		for _, s := range b.Stances {
			ids = append(ids, s.TopicID)
		}
		slices.Sort(ids)
		return ids
	}
	sorted := func(ids ...string) []string { slices.Sort(ids); return ids }

	for _, tt := range []struct {
		name     string
		caller   api.Caller
		entities []string
		topics   []string
	}{
		{"a person granted everything", kyle, []string{w.item, w.repo, w.engine}, sorted(w.ownTopic, w.engineTopic, w.repoTopic)},
		{"an observer", peekKyle, []string{w.item}, sorted(w.ownTopic)},
		{"a worker", shedForKyle, []string{w.item, w.engine}, sorted(w.ownTopic, w.engineTopic)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := bundleFor(tt.caller)
			if got := entities(b); !slices.Equal(got, tt.entities) {
				t.Errorf("entities = %v, want %v", got, tt.entities)
			}
			if got := topics(b); !slices.Equal(got, tt.topics) {
				t.Errorf("stances on %v, want %v", got, tt.topics)
			}
		})
	}
}

// Every handle answers an id out of the caller's reach exactly as it answers
// one that does not exist — status and body, byte for byte, over both
// interfaces — and serves it to a caller whose reach holds it. A not-found
// message names the id it was asked for, so the nonexistent id's body is
// compared with that id spelled as the out-of-reach one.
func TestAnOutOfReachIDIsAnsweredAsANonexistentOne(t *testing.T) {
	w := newReachWorld(t)
	nothing := w.src + "-nothing"
	for _, tt := range []struct {
		name, call, arg string
		// outside is an id out of the observer's reach that kyle reaches;
		// missing is one nothing holds.
		outside, missing string
		// holds is what the answer to kyle carries, to show it is there.
		holds string
	}{
		{"get_l1", "get_l1", "id", w.engineDoc, l1.DocID(w.src, nothing), "retries every write"},
		{"get_l1, a document about no entity", "get_l1", "id", w.orphan, l1.DocID(w.src, nothing), "nothing in particular"},
		{"get_l0", "get_l0", "id", connector.EventID(w.src, w.project+"#30"), connector.EventID(w.src, nothing), "retries every write"},
		{"get_l0, an event nothing was distilled from", "get_l0", "id", w.undistilled, connector.EventID(w.src, nothing), "Not distilled yet"},
		{"stance_history", "stance_history", "topic", w.engineTopic, "topic:" + nothing, "twice, then gives up"},
		{"stance_history, a topic about an ancestor", "stance_history", "topic", w.repoTopic, "topic:" + nothing, "as JSON"},
		{"get_bundle", "get_bundle", "scope", w.web, "code:" + nothing, "caches sessions"},
		{"search within a scope", "search", "scope", w.engine, "code:" + nothing, "retries every write"},
		{"resolve", "resolve", "text", w.src + "-engine", nothing, w.engine},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := func(v string) map[string]any {
				a := map[string]any{tt.arg: v}
				if tt.call == "search" {
					a["query"] = "retries every write"
				}
				return a
			}
			if overHTTP, overMCP := w.both(t, kyle, tt.call, args(tt.outside)); overHTTP.status != http.StatusOK ||
				overMCP.isError || !strings.Contains(overHTTP.body, tt.holds) || overHTTP.body != overMCP.body {
				t.Fatalf("%s(%s) for kyle: HTTP %+v MCP %+v, want it served with %q", tt.call, tt.outside, overHTTP, overMCP, tt.holds)
			}
			outHTTP, outMCP := w.both(t, peekKyle, tt.call, args(tt.outside))
			missHTTP, missMCP := w.both(t, peekKyle, tt.call, args(tt.missing))
			respell := func(a answer) answer {
				a.body = strings.ReplaceAll(a.body, tt.missing, tt.outside)
				return a
			}
			if outHTTP != respell(missHTTP) {
				t.Errorf("over HTTP, out of reach = %+v, nonexistent = %+v", outHTTP, missHTTP)
			}
			if outMCP != respell(missMCP) {
				t.Errorf("over MCP, out of reach = %+v, nonexistent = %+v", outMCP, missMCP)
			}
			if strings.Contains(outHTTP.body, tt.holds) {
				t.Errorf("the observer was served %q: %s", tt.holds, outHTTP.body)
			}
		})
	}
}

// Search without a scope ranks only what is in reach.
func TestSearchRanksOnlyWhatIsInReach(t *testing.T) {
	w := newReachWorld(t)
	found := func(caller api.Caller) []string {
		t.Helper()
		overHTTP, overMCP := w.both(t, caller, "search", map[string]any{"query": "engine retries every write"})
		if overHTTP.status != http.StatusOK || overHTTP.body != overMCP.body {
			t.Fatalf("search for %+v: HTTP %+v MCP %+v", caller, overHTTP, overMCP)
		}
		var out struct {
			Documents []l1.Document `json:"documents"`
		}
		_ = json.Unmarshal([]byte(overHTTP.body), &out)
		var ids []string
		for _, d := range out.Documents {
			ids = append(ids, d.ID)
		}
		return ids
	}
	if got := found(shedForKyle); !slices.Contains(got, w.engineDoc) {
		t.Errorf("the worker's search = %v, want %s", got, w.engineDoc)
	}
	if got := found(peekKyle); slices.Contains(got, w.engineDoc) {
		t.Errorf("the observer's search = %v, holds %s out of its reach", got, w.engineDoc)
	}
}

// The audit record names the agent's class and counts what reach withheld
// apart from what the access lists did.
func TestTheAuditRecordCountsWhatReachWithheld(t *testing.T) {
	w := newReachWorld(t)
	w.both(t, shedForKyle, "get_bundle", map[string]any{"scope": w.item})
	w.both(t, peekKyle, "get_bundle", map[string]any{"scope": w.web})
	w.both(t, annAlone, "get_bundle", map[string]any{"scope": w.item})

	events, err := w.events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: api.AuditSource, Kind: connector.KindAudit}, Newest: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]api.AuditRecord{}
	for _, ev := range events {
		var rec api.AuditRecord
		if err := json.Unmarshal(ev.Payload.Native, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Scope == w.item || rec.Scope == w.web {
			// Two calls each: one over HTTP and one over MCP.
			got[rec.Principal+"/"+rec.Agent+"@"+rec.Scope] = rec
		}
	}
	for _, tt := range []struct {
		key      string
		class    string
		reach    bundle.Reach
		withheld bundle.Withheld
	}{
		// The worker drops the repository entity and the stance inherited
		// from it; kyle's access lists withhold nothing.
		{"kyle/shed@" + w.item, "worker", bundle.Reach{Stances: 1, Entities: 1}, bundle.Withheld{}},
		// The web directory is out of the observer's reach: its whole bundle
		// is, and the one document about it is counted.
		{"kyle/peek@" + w.web, "observer", bundle.Reach{Scope: true, Documents: 1}, bundle.Withheld{}},
		// ann reaches the engine but not the repository; the private
		// follow-up is in her reach and withheld by its access list.
		{"ann/@" + w.item, "", bundle.Reach{Stances: 1, Entities: 1}, bundle.Withheld{Documents: 1}},
	} {
		rec, ok := got[tt.key]
		if !ok {
			t.Errorf("no audit record for %s among %v", tt.key, got)
			continue
		}
		if rec.Class != tt.class || rec.Report.Reach != tt.reach || rec.Report.Withheld != tt.withheld {
			t.Errorf("%s: class %q, reach %+v, withheld %+v; want %q, %+v, %+v",
				tt.key, rec.Class, rec.Report.Reach, rec.Report.Withheld, tt.class, tt.reach, tt.withheld)
		}
	}
}
