//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

type reachWorld struct {
	src, project, item     string
	repo, engine, web      string
	pr, engineDoc, webDoc  string
	readme, orphan, secret string
	undistilled            string
	ownTopic, engineTopic  string
	repoTopic              string
	client
	events *l0.Store
}

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
	pool := dbtest.Pool(t)
	ctx := t.Context()
	w := &reachWorld{src: dbtest.UniqueID(), events: l0.New(pool), client: client{credential: reachCredential}}
	w.project = "acme/" + w.src
	w.repo, w.engine, w.web = "code:"+w.project, "code:"+w.project+":engine", "code:"+w.project+":web"
	w.item = "tracker:" + w.src + ":" + w.project + "#20"
	docs, graph := l1.New(pool), l2.New(pool)

	public := connector.ACL{{Kind: connector.ACLPublic}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}}
	put := func(artifact string, kind l1.Kind, hour int, text string, acl connector.ACL, scope []string, refs ...l1.Reference) string {
		t.Helper()
		return putDocument(t, w.events, docs, documentSpec{
			src: w.src, project: w.project, artifact: artifact, kind: kind, hour: hour, text: text, acl: acl, scope: scope, refs: refs,
		})
	}
	w.pr = put(w.project+"#20", l1.KindPR, 1, "Moves the lock into the worker.", public,
		[]string{w.repo, w.engine, w.item}, l1.Reference{Type: l1.RefSystem, ID: w.engine})
	w.engineDoc = put(w.project+"#30", l1.KindIssue, 2, "The engine retries every write twice.", public, []string{w.repo, w.engine})
	w.webDoc = put(w.project+"#31", l1.KindIssue, 3, "The web client caches sessions.", public, []string{w.repo, w.web})
	w.readme = put(w.project+"#32", l1.KindIssue, 4, "Every service logs as JSON.", public, []string{w.repo})
	w.orphan = put(w.project+"#33", l1.KindIssue, 5, "A note about nothing in particular.", public, nil)
	w.secret = put(w.project+"#34", l1.KindIssue, 6, "The pull request's private follow-up.", private, []string{w.item})

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

func (w *reachWorld) reads(t *testing.T, caller api.Caller, doc string) bool {
	t.Helper()
	overHTTP, overMCP := w.both(t, caller, "get_l1", map[string]any{"id": doc})
	if overHTTP.body != overMCP.body || (overHTTP.status == http.StatusOK) == overMCP.isError {
		t.Fatalf("get_l1(%s) for %+v: HTTP %+v and MCP %+v disagree", doc, caller, overHTTP, overMCP)
	}
	return overHTTP.status == http.StatusOK
}

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
		name:   "a person and an agent reach only what both do",
		caller: pryAnn,
		reads:  []string{w.pr},
		denied: []string{w.engineDoc, w.webDoc, w.readme, w.secret},
	}, {
		name:   "a steward reads everything the person does, beyond its own scopes",
		caller: bossKyle,
		reads:  []string{w.pr, w.engineDoc, w.webDoc, w.readme, w.orphan, w.secret},
	}, {
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

func TestAnOutOfReachIDIsAnsweredAsANonexistentOne(t *testing.T) {
	w := newReachWorld(t)
	nothing := w.src + "-nothing"
	for _, tt := range []struct {
		name, call, arg string

		outside, missing string

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
			got[rec.Principal+"/"+rec.Agent+"@"+rec.Scope] = rec
		}
	}
	for _, tt := range []struct {
		key      string
		class    string
		reach    bundle.Reach
		withheld bundle.Withheld
	}{
		{"kyle/shed@" + w.item, "worker", bundle.Reach{Stances: 1, Entities: 1}, bundle.Withheld{}},
		{"kyle/peek@" + w.web, "observer", bundle.Reach{Scope: true, Documents: 1}, bundle.Withheld{}},
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
