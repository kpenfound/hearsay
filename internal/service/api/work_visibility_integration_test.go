//go:build integration

package api_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

type workVisibilityWorld struct {
	*reachWorld
	pool                        *pgxpool.Pool
	docs                        *l1.Store
	graph                       *l2.Store
	calls                       *api.Calls
	projectEntity, streamEntity string
}

func newWorkVisibilityWorld(t *testing.T, repos ...string) *workVisibilityWorld {
	t.Helper()
	pool, err := db.Connect(t.Context(), api.ScratchDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w := &workVisibilityWorld{reachWorld: &reachWorld{src: "github", repo: "code:acme/api", events: l0.New(pool)}, pool: pool, docs: l1.New(pool), graph: l2.New(pool), projectEntity: "work:projects:retry", streamEntity: "work:projects:retry#policy"}
	for _, e := range []l2.Entity{
		{ID: w.repo, Type: l2.TypeProject, Name: "API repository", Origin: l2.OriginConfig},
		{ID: w.projectEntity, Type: l2.TypeProject, Name: "Resilient delivery", Origin: l2.OriginSource, PartOf: []string{w.repo}},
		{ID: w.streamEntity, Type: l2.TypeTrackerItem, Name: "Retry policy", Origin: l2.OriginSource, PartOf: []string{w.projectEntity}},
		{ID: "code:reference", Type: l2.TypeModule, Name: "Reference engine", Origin: l2.OriginReference, PartOf: []string{w.repo}},
		{ID: "code:structure", Type: l2.TypeModule, Name: "Structure engine", Origin: l2.OriginRepoStructure, PartOf: []string{w.repo}},
	} {
		if err := w.graph.PutEntity(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	ps := []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.SomeScopes(w.repo)}},
		{ID: "ann", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.SomeScopes(w.repo)}},
		{ID: "outside", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.SomeScopes("code:elsewhere")}},
		{ID: "all", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.AllScopes()}},
		{ID: "api-team", Kind: principal.KindTeam, Identities: []principal.Identity{{Source: "github", NativeID: "acme/api"}}, Members: []string{"kyle", "all", "outside"}},
	}
	for i := range ps {
		if ps[i].Kind == principal.KindTeam {
			continue
		}
		env := "HEARSAY_WORK_VISIBILITY_" + strings.ToUpper(ps[i].ID)
		ps[i].TokenEnv = env
		t.Setenv(env, "reach-"+ps[i].ID+"-credential")
	}
	w.calls, err = api.NewCalls(pool, config.Repo{Principals: ps, Sources: []connector.SourceConfig{{ID: "projects", Type: "work", Containers: []string{"*"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.calls.WithWatch(15*time.Millisecond, 2*time.Millisecond)
	w.server = httptest.NewServer(api.Handler(w.calls, pool))
	t.Cleanup(w.server.Close)
	var refs []connector.ContainerRef
	for _, r := range repos {
		refs = append(refs, connector.ContainerRef{Source: "github", NativeID: r})
	}
	containers, key, err := connector.CanonicalContainers(refs)
	if err != nil {
		t.Fatal(err)
	}
	acl := connector.ACL{{Kind: connector.ACLContainers, NativeID: key, Containers: containers}}
	w.put(t, "projects", "retry", acl, []string{w.repo, w.projectEntity}, "Resilient delivery")
	w.put(t, "projects", "retry#policy", acl, []string{w.repo, w.projectEntity, w.streamEntity}, "Retry policy")
	// Readable evidence about the hidden entities must not make a scoped call
	// reveal them. Its scope also exercises related bundle entity listings.
	w.put(t, "github", "public-note", openACL, []string{w.repo, w.projectEntity, w.streamEntity}, "visibilityproof")
	return w
}

func (w *workVisibilityWorld) put(t *testing.T, source, artifact string, acl connector.ACL, scope []string, title string) {
	t.Helper()
	ev := connector.NewFake(connector.SourceConfig{ID: source}).NewEvent(connector.KindIssue, artifact, title)
	ev.Payload.Container = connector.Container{Kind: "project", NativeID: "retry"}
	ev.ACL = acl
	if _, err := w.events.Append(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	d := l1.Document{ID: l1.DocID(source, artifact), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue, Source: l1.Source{System: source, NativeID: artifact}, L0Refs: []string{connector.EventID(source, artifact)}, Time: l1.Times{Created: ev.Time, Updated: ev.Time, LastActivity: ev.Time}, Scope: scope, ACL: acl, Text: title, RawText: title, Body: l1.Body{Summary: title, OutcomeKind: l1.OutcomeNone}}
	if _, err := w.docs.Put(t.Context(), d); err != nil {
		t.Fatal(err)
	}
}

func (w *workVisibilityWorld) repositoryRevision(t *testing.T, repo, artifact, revision string, hour int, acl connector.ACL) {
	t.Helper()
	ev := connector.NewFake(connector.SourceConfig{ID: "github"}).NewEvent(connector.KindIssue, artifact+"@"+revision, "Repository content")
	ev.Payload.Artifact = artifact
	ev.Payload.Revision = &connector.Revision{Token: revision, EditedAt: ev.Time.Add(time.Duration(hour) * time.Hour)}
	ev.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: repo}
	ev.ACL = acl
	if _, err := w.events.Append(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
}

func (w *workVisibilityWorld) resolves(t *testing.T, caller api.Caller, name, id string, want bool) {
	t.Helper()
	h, m := w.both(t, caller, "resolve", map[string]string{"text": name})
	if h.status != http.StatusOK || m.isError || h.body != m.body {
		t.Fatalf("resolve: HTTP %+v MCP %+v", h, m)
	}
	var result struct {
		Entities []api.EntityMatch `json:"entities"`
	}
	if err := json.Unmarshal([]byte(h.body), &result); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range result.Entities {
		found = found || e.ID == id
	}
	if found != want {
		t.Fatalf("resolve(%q) for %s = %s, want found=%v", name, caller.Principal, h.body, want)
	}
}

func (w *workVisibilityWorld) hiddenScope(t *testing.T, caller api.Caller, entity string) {
	t.Helper()
	// Start from the same settled cursor for both requests; no wall-clock field
	// or feed position may distinguish a hidden entity from a missing one.
	start, _ := w.both(t, caller, "watch", map[string]any{"scope": "work:projects:absent"})
	var watched api.Watched
	if err := json.Unmarshal([]byte(start.body), &watched); err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"get_bundle", "search", "watch"} {
		t.Run(call, func(t *testing.T) {
			args := func(id string) map[string]any {
				a := map[string]any{"scope": id}
				if call == "search" {
					a["query"] = "visibilityproof"
				}
				if call == "watch" {
					a["after"] = watched.Cursor
				}
				return a
			}
			h, m := w.both(t, caller, call, args(entity))
			mh, mm := w.both(t, caller, call, args("work:projects:absent"))
			respell := func(a answer) answer { a.body = strings.ReplaceAll(a.body, "work:projects:absent", entity); return a }
			if h != respell(mh) || m != respell(mm) {
				t.Fatalf("%s hidden HTTP %+v MCP %+v; missing HTTP %+v MCP %+v", call, h, m, mh, mm)
			}
		})
	}
}

func TestWorkEntityVisibilityUsesCurrentDocuments(t *testing.T) {
	for _, tt := range []struct {
		name                string
		repos               []string
		change              string
		annReads, kyleReads bool
	}{
		{"private repository", []string{"acme/api"}, "private", false, true},
		{"two repositories only one readable", []string{"acme/api", "acme/other"}, "two", false, true},
		{"missing repository", []string{"acme/api", "acme/missing"}, "private", false, false},
		{"not distilled", []string{"acme/api"}, "undistilled", false, false},
		{"tombstoned before distiller catches up", []string{"acme/api"}, "tombstone", false, false},
		{"current document access changed", []string{"acme/api"}, "acl", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorkVisibilityWorld(t, tt.repos...)
			private := connector.ACL{{Kind: connector.ACLGroup, Source: "github", NativeID: "acme/api"}}
			w.repositoryRevision(t, "acme/api", "api-issue", "private", 1, private)
			switch tt.change {
			case "two":
				w.repositoryRevision(t, "acme/api", "api-issue", "public", 2, openACL)
				w.repositoryRevision(t, "acme/other", "other-issue", "private", 1, private)
			case "undistilled":
				for _, a := range []string{"retry", "retry#policy"} {
					if _, err := w.docs.Delete(t.Context(), l1.DocID("projects", a)); err != nil {
						t.Fatal(err)
					}
				}
			case "tombstone":
				for _, a := range []string{"retry", "retry#policy"} {
					ev := connector.NewFake(connector.SourceConfig{ID: "projects"}).NewEvent(connector.KindTombstone, a+":deleted", "")
					ev.Payload.Target = a
					if _, err := w.events.Append(t.Context(), ev); err != nil {
						t.Fatal(err)
					}
				}
			case "acl":
				for _, a := range []string{"retry", "retry#policy"} {
					d, err := w.docs.Get(t.Context(), l1.DocID("projects", a))
					if err != nil {
						t.Fatal(err)
					}
					d.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: "projects", NativeID: "kyle"}}
					if _, err := w.docs.Put(t.Context(), d.Document); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, c := range []struct {
				who   string
				reads bool
			}{{"kyle", tt.kyleReads}, {"ann", tt.annReads}, {"outside", false}} {
				caller := api.Caller{Principal: c.who}
				for _, e := range []struct{ name, id string }{{"Resilient delivery", w.projectEntity}, {"Retry policy", w.streamEntity}} {
					w.resolves(t, caller, e.name, e.id, c.reads)
					if !c.reads {
						w.hiddenScope(t, caller, e.id)
					}
				}
			}
			if !tt.kyleReads { // Even an unrestricted reader must establish document access.
				w.resolves(t, api.Caller{Principal: "all"}, "Retry policy", w.streamEntity, false)
				w.hiddenScope(t, api.Caller{Principal: "all"}, w.streamEntity)
			}
			for _, origin := range []struct{ name, id string }{{"API repository", w.repo}, {"Reference engine", "code:reference"}, {"Structure engine", "code:structure"}} {
				w.resolves(t, annAlone, origin.name, origin.id, true)
			}
		})
	}
}

// A readable document about a different entity cannot disclose a hidden work
// entity through its scope, references, or the bundle's related entities.
func TestWorkEntityVisibilityFiltersRelatedListings(t *testing.T) {
	w := newWorkVisibilityWorld(t, "acme/api")
	private := connector.ACL{{Kind: connector.ACLGroup, Source: "github", NativeID: "acme/api"}}
	w.repositoryRevision(t, "acme/api", "repo-issue", "private", 1, private)
	subject := "tracker:github:public-note"
	if err := w.graph.PutEntity(t.Context(), l2.Entity{ID: subject, Type: l2.TypeTrackerItem, Name: "Public note", PartOf: []string{w.repo}, Origin: l2.OriginReference}); err != nil {
		t.Fatal(err)
	}
	doc, err := w.docs.Get(t.Context(), l1.DocID("github", "public-note"))
	if err != nil {
		t.Fatal(err)
	}
	doc.Scope = append(doc.Scope, subject)
	doc.References = []l1.Reference{{Type: l1.RefSystem, ID: w.projectEntity}, {Type: l1.RefSystem, ID: w.repo}}
	if _, err := w.docs.Put(t.Context(), doc.Document); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		call string
		args map[string]string
	}{
		{"get_bundle", map[string]string{"scope": subject}},
		{"get_l1", map[string]string{"id": doc.ID}},
		{"search", map[string]string{"query": "visibilityproof"}},
	} {
		t.Run(tt.call, func(t *testing.T) {
			for _, who := range []string{"ann", "kyle"} {
				h, m := w.both(t, api.Caller{Principal: who}, tt.call, tt.args)
				if h.status != http.StatusOK || m.isError || h.body != m.body {
					t.Fatalf("HTTP %+v MCP %+v", h, m)
				}
				for _, id := range []string{w.projectEntity, w.streamEntity} {
					if strings.Contains(h.body, id) != (who == "kyle") {
						t.Fatalf("%s for %s: %s", tt.call, who, h.body)
					}
				}
			}
		})
	}
}

// githubVisibilitySink records the real repository.privatized request and
// sends the connector's real re-sync emissions to L0, one page at a time.
type githubVisibilitySink struct {
	events  *l0.Store
	resyncs *l0.Resyncs
}

func (s githubVisibilitySink) Emit(ctx context.Context, ev connector.Event) error {
	_, err := s.events.Append(ctx, ev)
	return err
}
func (s githubVisibilitySink) RequestResync(ctx context.Context, container string) error {
	return s.resyncs.Owe(ctx, "github", container)
}

func TestWorkEntityVisibilityFollowsGitHubPrivatizationWithoutRepost(t *testing.T) {
	w := newWorkVisibilityWorld(t, "acme/api")
	var private atomic.Bool
	// A two-page issue walk deliberately leaves mixed ACLs between pages.
	rest := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api":
			_ = json.NewEncoder(rw).Encode(map[string]any{"full_name": "acme/api", "private": private.Load(), "default_branch": "main"})
		case "/repos/acme/api/issues":
			n := 1
			if r.URL.Query().Get("since") != "" {
				n = 2
			} else {
				rw.Header().Set("Link", `<http://example.invalid/next>; rel="next"`)
			}
			_ = json.NewEncoder(rw).Encode([]any{map[string]any{"number": n, "title": "repository issue", "body": "body", "state": "open", "created_at": day, "updated_at": day, "user": map[string]string{"login": "kyle", "node_id": kyleNode, "type": "User"}}})
		case "/repos/acme/api/pulls", "/repos/acme/api/issues/comments", "/repos/acme/api/pulls/comments":
			_, _ = rw.Write([]byte("[]"))
		case "/repos/acme/api/branches/main":
			rw.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected GitHub request %s", r.URL)
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	defer rest.Close()
	settings, _ := json.Marshal(map[string]string{"api_url": rest.URL})
	gh, err := github.New(connector.SourceConfig{ID: "github", Type: "github", Containers: []string{"acme/api"}, Settings: settings, Secrets: map[string]string{"token": "test-token", "webhook_secret": "test-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	sink := githubVisibilitySink{w.events, l0.NewResyncs(w.pool)}
	walk := func(from connector.Cursor) connector.BackfillResult {
		t.Helper()
		result, err := gh.Resync(t.Context(), sink, "acme/api", from)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cursor := connector.Cursor("")
	for i := 0; i < 10; i++ {
		r := walk(cursor)
		if r.Done {
			break
		}
		cursor = r.Next
		if i == 9 {
			t.Fatal("public walk did not finish")
		}
	}
	workBefore, err := w.events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: "projects"}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		advance  int
		annReads bool
	}{
		{"before privatization", 0, true},
		{"private revision during resync", 1, false},
		{"after resync", 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.advance == 1 {
				private.Store(true)
				body := []byte(`{"action":"privatized","repository":{"full_name":"acme/api","private":true}}`)
				mac := hmac.New(sha256.New, []byte("test-secret"))
				_, _ = mac.Write(body)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", bytes.NewReader(body))
				req.Header.Set("X-GitHub-Event", "repository")
				req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
				response := httptest.NewRecorder()
				gh.Handler(sink).ServeHTTP(response, req)
				if response.Code != http.StatusAccepted {
					t.Fatalf("privatized = %d %s", response.Code, response.Body.String())
				}
				owed, err := sink.resyncs.Resyncs(t.Context(), "github")
				if err != nil || len(owed) != 1 {
					t.Fatalf("resyncs=%v,%v", owed, err)
				}
				r := walk("")
				if r.Done {
					t.Fatal("first private page ended resync")
				}
				cursor = r.Next
				lists, err := w.events.ContainerAccess(t.Context(), "github", "acme/api")
				if err != nil || len(lists) != 2 {
					t.Fatalf("during resync access lists=%v,%v; want public and private", lists, err)
				}
			}
			if tt.advance == 2 {
				for i := 0; i < 10; i++ {
					r := walk(cursor)
					if r.Done {
						break
					}
					cursor = r.Next
					if i == 9 {
						t.Fatal("private walk did not finish")
					}
				}
			}
			for _, c := range []struct {
				who   string
				reads bool
			}{{"kyle", true}, {"ann", tt.annReads}} {
				for _, artifact := range []string{"retry", "retry#policy"} {
					w.documentAccess(t, c.who, artifact, c.reads)
				}
				for _, e := range []struct{ name, id string }{{"Resilient delivery", w.projectEntity}, {"Retry policy", w.streamEntity}} {
					w.resolves(t, api.Caller{Principal: c.who}, e.name, e.id, c.reads)
					if !c.reads {
						w.hiddenScope(t, api.Caller{Principal: c.who}, e.id)
					} else {
						// Prime the public bundles before privatization, then read
						// through the same cache after repository access changes.
						for _, call := range []string{"get_bundle", "search"} {
							args := map[string]string{"scope": e.id}
							if call == "search" {
								args["query"] = "visibilityproof"
							}
							h, m := w.both(t, api.Caller{Principal: c.who}, call, args)
							if h.status != http.StatusOK || m.isError || h.body != m.body || !strings.Contains(h.body, l1.DocID("github", "public-note")) {
								t.Fatalf("%s on readable scope for %s: HTTP %+v MCP %+v", call, c.who, h, m)
							}
						}
					}
				}
			}
			after, err := w.events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: "projects"}, Limit: l0.MaxLimit})
			if err != nil || len(after) != len(workBefore) {
				t.Fatalf("work source count=%d,%v, want unchanged %d", len(after), err, len(workBefore))
			}
		})
	}
}

func TestWorkEntityVisibilityAlsoFiltersHistoricalSessionListings(t *testing.T) {
	w := newWorld(t)
	source, session := w.src+"session", "visibility-session"
	ev := connector.NewFake(connector.SourceConfig{ID: source}).NewEvent(connector.KindAgentSession, session+"@start", "")
	ev.Payload.Artifact = session
	ev.Payload.Revision = &connector.Revision{Token: "start", EditedAt: ev.Time}
	ev.Payload.Author = &connector.Identity{Source: source, Kind: connector.IdentityAgent, NativeID: "shed"}
	ev.Payload.Participants = []connector.Participant{{Identity: connector.Identity{Source: source, Kind: connector.IdentityUser, NativeID: "kyle"}, Role: connector.RoleAuthor}}
	if _, err := l0.New(w.pool).Append(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	id := "work:" + w.src + ":private-project"
	if err := l2.New(w.pool).PutEntity(t.Context(), l2.Entity{ID: id, Type: l2.TypeProject, Name: "Private project", Origin: l2.OriginSource}); err != nil {
		t.Fatal(err)
	}
	doc := w.putIn(id, "private-project", l1.KindIssue, 8, "Private project", openACL)
	for i, record := range []api.AuditRecord{
		{Call: "get_bundle", Scope: id},
		{Call: "get_bundle", Scope: "work:" + w.src + ":missing-work"},
		{Call: "resolve", ReturnedIDs: []string{id}},
		{Call: "get_bundle", Sections: map[string]api.AuditIDs{"scope.entities": {Entities: []string{id}}}},
	} {
		record.Principal = "kyle"
		record.Agent = "shed"
		record.Session = session
		record.SessionSource = source
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		audit := connector.NewFake(connector.SourceConfig{ID: api.AuditSource}).NewEvent(connector.KindAudit, w.src+fmt.Sprint(i), "")
		audit.Payload.Native = raw
		if _, err := l0.New(w.pool).Append(t.Context(), audit); err != nil {
			t.Fatal(err)
		}
	}
	caller := api.Caller{Principal: "kyle", Agent: "shed", Session: session}
	var asserted api.Asserted
	if err := json.Unmarshal(w.http(t, caller, "assert", map[string]any{"topic": w.lockTopic(), "position": "Use the worker lock", "evidence": []string{w.issue}}), &asserted); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		remove bool
		want   bool
	}{{"readable entity", false, true}, {"document removed", true, false}} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.remove {
				if _, err := l1.New(w.pool).Delete(t.Context(), doc); err != nil {
					t.Fatal(err)
				}
			}
			h := w.http(t, kyle, "get_session", map[string]string{"assertion": asserted.ID})
			m, isError := w.mcp(t, kyle, "get_session", map[string]string{"assertion": asserted.ID})
			if isError || !bytes.Equal(h, m) || bytes.Contains(h, []byte(id)) != tt.want || bytes.Contains(h, []byte("missing-work")) {
				t.Fatalf("get_session HTTP %s MCP %s isError=%v, want entity=%v", h, m, isError, tt.want)
			}
		})
	}
}

func TestWorkEntityVisibilityGuardsWatchWithReadableEvidence(t *testing.T) {
	w := newWorkVisibilityWorld(t, "acme/api")
	w.repositoryRevision(t, "acme/api", "repo-issue", "private", 1, connector.ACL{{Kind: connector.ACLGroup, Source: "github", NativeID: "acme/api"}})
	baseline, _ := w.both(t, annAlone, "watch", map[string]string{"scope": "work:projects:absent"})
	var start api.Watched
	if err := json.Unmarshal([]byte(baseline.body), &start); err != nil {
		t.Fatal(err)
	}
	w.put(t, "github", "watch-note", openACL, []string{w.repo, w.projectEntity, w.streamEntity}, "watch visibility proof")
	// Reuse the feed-settling setup used by the API's watch integration tests.
	(&watchWorld{pool: w.pool}).pumped(t)
	for _, entity := range []string{w.projectEntity, w.streamEntity} {
		args := func(scope string) map[string]string { return map[string]string{"scope": scope, "after": start.Cursor} }
		ownerHTTP, ownerMCP := w.both(t, api.Caller{Principal: "kyle"}, "watch", args(entity))
		if ownerHTTP.status != http.StatusOK || ownerMCP.isError || ownerHTTP.body != ownerMCP.body || !strings.Contains(ownerHTTP.body, l1.DocID("github", "watch-note")) {
			t.Fatalf("readable scope watch HTTP %+v MCP %+v", ownerHTTP, ownerMCP)
		}
		hiddenHTTP, hiddenMCP := w.both(t, annAlone, "watch", args(entity))
		missingHTTP, missingMCP := w.both(t, annAlone, "watch", args("work:projects:absent"))
		if hiddenHTTP != missingHTTP || hiddenMCP != missingMCP {
			t.Fatalf("hidden scope watch HTTP %+v MCP %+v; missing HTTP %+v MCP %+v", hiddenHTTP, hiddenMCP, missingHTTP, missingMCP)
		}
	}
}
