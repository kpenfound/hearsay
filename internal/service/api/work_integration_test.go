//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// Explicit recording uses authored answers below and the exported prompt
// builders, as in the service fixture suites. Normal runs only replay disk.
var recordWork = flag.Bool("record-work", false, "record work integration model exchanges; never calls a provider")

type postedWork struct {
	*workVisibilityWorld
	cfg  config.Config
	hook http.Handler
}

func newPostedWork(t *testing.T) *postedWork {
	t.Helper()
	pool, err := db.Connect(t.Context(), api.ScratchDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := config.Default()
	cfg.Repo = config.Repo{
		Sources: []connector.SourceConfig{
			{ID: "projects", Type: "work", Containers: []string{"*"}, Settings: json.RawMessage(`{"posters":["factory","courier"],"attributors":["factory","courier"]}`)},
			{ID: "github", Type: "github", Containers: []string{"acme/api", "acme/other"}},
		},
		Scopes: []config.Scope{{ID: "api", Sources: []config.ScopeSource{{Source: "github", Containers: []string{"acme/api"}}}}},
		Code:   []config.CodeEntity{{ID: "code:acme/api", Type: config.TypeProject, Name: "API repository", Repo: config.SourceRef{Source: "github", Project: "acme/api"}}, {ID: "code:acme/other", Type: config.TypeProject, Name: "Other repository", Repo: config.SourceRef{Source: "github", Project: "acme/other"}}},
		Principals: []principal.Principal{
			{ID: "factory", Kind: principal.KindAgent, Class: principal.ClassObserver, TokenEnv: "HEARSAY_WORK_FACTORY", Grant: principal.Grant{Scopes: principal.AllScopes()}},
			{ID: "courier", Kind: principal.KindAgent, Class: principal.ClassObserver, TokenEnv: "HEARSAY_WORK_COURIER", Grant: principal.Grant{Scopes: principal.AllScopes()}},
			{ID: "kyle", Kind: principal.KindHuman, TokenEnv: "HEARSAY_WORK_KYLE", Grant: principal.Grant{Scopes: principal.SomeScopes("code:acme/api")}},
			{ID: "ann", Kind: principal.KindHuman, TokenEnv: "HEARSAY_WORK_ANN", Grant: principal.Grant{Scopes: principal.SomeScopes("code:acme/api")}},
			{ID: "outside", Kind: principal.KindHuman, TokenEnv: "HEARSAY_WORK_OUTSIDE", Grant: principal.Grant{Scopes: principal.SomeScopes("code:elsewhere")}},
			{ID: "api-team", Kind: principal.KindTeam, Identities: []principal.Identity{{Source: "github", NativeID: "acme/api"}}, Members: []string{"kyle", "outside"}},
		}, LLM: llm.Default(),
	}
	for _, p := range cfg.Repo.Principals {
		if p.TokenEnv != "" {
			t.Setenv(p.TokenEnv, "reach-"+p.ID+"-credential")
		}
	}
	// Load authority through its public configuration surface.
	file := filepath.Join(t.TempDir(), "hearsay.yaml")
	if err := os.WriteFile(file, []byte("sources:\n  - id: projects\n    type: work\n    containers: [\"*\"]\nscopes:\n  - id: api\n    sources: [projects]\nprincipals:\n  - id: kyle\n    kind: human\nauthority:\n  - scope: '*'\n    ratified_by:\n      principals: [kyle]\n      sources: []\n      artifacts: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repo.Authority = loaded.Authority
	c, err := work.New(cfg.Repo.Sources[0], cfg.Repo.Principals, []string{"projects", "github"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	events := l0.New(pool)
	gate := connector.NewGate(events, "projects", c.Describe(), connector.NewAllowlist(cfg.Repo.Sources...))
	calls, err := api.NewCalls(pool, cfg.Repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls.WithWatch(15*time.Millisecond, 2*time.Millisecond)
	server := httptest.NewServer(api.Handler(calls, pool))
	t.Cleanup(server.Close)
	w := &postedWork{workVisibilityWorld: &workVisibilityWorld{reachWorld: &reachWorld{src: "github", repo: "code:acme/api", events: events, server: server}, pool: pool, docs: l1.New(pool), graph: l2.New(pool), calls: calls, projectEntity: "work:projects:retry", streamEntity: "work:projects:retry#policy"}, cfg: cfg, hook: c.Handler(gate)}
	w.repositoryRevision(t, "acme/api", "api-issue", "private", 1, connector.ACL{{Kind: connector.ACLGroup, Source: "github", NativeID: "acme/api"}})
	w.repositoryRevision(t, "acme/other", "other-issue", "private", 1, connector.ACL{{Kind: connector.ACLGroup, Source: "github", NativeID: "acme/other"}})
	return w
}

func (w *postedWork) postWork(t *testing.T, raw string) string {
	t.Helper()
	return w.postWorkAs(t, "factory", raw)
}

func (w *postedWork) postWorkAs(t *testing.T, poster, raw string) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/hooks/projects", strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer reach-"+poster+"-credential")
	response := httptest.NewRecorder()
	w.hook.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("post: %d %s", response.Code, response.Body.String())
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.ID
}

const workProject = `{"kind":"project","project":"retry","name":"Resilient delivery","description":"Improve delivery reliability.","repos":[{"source":"github","project":"acme/api"}],"created_at":"2026-10-01T12:00:00Z"}`
const workStream = `{"kind":"workstream","project":"retry","workstream":"policy","name":"Retry policy","description":"Choose the retry limit.","touches":[{"source":"github","project":"acme/api"}],"created_at":"2026-10-01T13:00:00Z"}`
const workUpdate = `{"kind":"update","project":"retry","workstream":"policy","update":"decision","text":"Retry twice, then stop.","created_at":"2026-10-01T14:00:00Z"}`

func (w *postedWork) current(t *testing.T, artifact string) connector.Event {
	t.Helper()
	ev, ok, err := w.events.CurrentArtifact(t.Context(), "projects", artifact)
	if err != nil || !ok {
		t.Fatalf("current %s: %v,%v", artifact, ok, err)
	}
	return ev
}

// Each fixture contains the exact request as well as the answer, so a prompt
// change fails playback until an explicit recording is reviewed.
func workRegistry(t *testing.T, name string, recordings []llm.CompletionFixture) llm.Registry {
	t.Helper()
	path := filepath.Join("testdata", "work", name+".json")
	if *recordWork {
		raw, err := json.MarshalIndent(llm.FixtureFile{Completions: recordings}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := llm.LoadFixtures(fstest.MapFS{filepath.Base(path): &fstest.MapFile{Data: raw}})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := llm.NewFake(llm.Default(), fixtures)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func (w *postedWork) distillWork(t *testing.T, name string, decided bool) l1.Document {
	t.Helper()
	root := w.current(t, name)
	var children []connector.Event
	if name == "retry#policy" {
		all, err := w.events.CurrentArtifacts(t.Context(), "projects")
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range all {
			if ev.Payload.Parent == name {
				children = append(children, ev)
			}
		}
	}
	resolver, err := w.cfg.Repo.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := l1.Build(l1.Input{Root: root, Children: children, Repo: w.cfg.Repo, Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	decided = decided || len(children) > 0
	body := l1.Body{Summary: "Delivery reliability work is planned.", OutcomeKind: l1.OutcomeNone}
	if decided {
		body = l1.Body{Summary: "The retry policy limits attempts.", Outcome: "Retry twice, then stop.", OutcomeKind: l1.OutcomeDecided}
	}
	raw, _ := json.Marshal(body)
	budget, _ := w.cfg.Repo.LLM.Tier(llm.TierDistill)
	fixtureName := strings.ReplaceAll(name, "#", "-")
	if decided {
		fixtureName += "-decision"
		if root.Payload.Author.NativeID == "kyle" || len(children) > 0 && children[0].Payload.Author.NativeID == "kyle" {
			fixtureName += "-human"
		}
	}
	registry := workRegistry(t, fixtureName, []llm.CompletionFixture{{Tier: llm.TierDistill, Request: distiller.RequestFor(expected, budget.MaxTokens), Response: llm.Response{JSON: raw, StopReason: llm.StopEnd}}})
	d, err := distiller.New(w.pool, registry, &w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := d.Distill(t.Context(), expected.ID); err != nil || result.Skipped || result.Superseded {
		t.Fatalf("distill: %+v %v", result, err)
	}
	got, err := w.docs.Get(t.Context(), expected.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Document
}

func (w *postedWork) placeWork(t *testing.T) {
	t.Helper()
	if _, err := assertworker.SeedEntities(t.Context(), w.pool, w.cfg.Repo, nil); err != nil {
		t.Fatal(err)
	}
}

func (w *workVisibilityWorld) documentAccess(t *testing.T, who, artifact string, want bool) {
	t.Helper()
	caller := api.Caller{Principal: who}
	id := l1.DocID("projects", artifact)
	if got := w.reads(t, caller, id); got != want {
		t.Fatalf("%s reads %s = %v, want %v", who, id, got, want)
	}
	if !want {
		for _, call := range []string{"get_l1", "get_l0"} {
			hidden, missing := id, l1.DocID("projects", "absent")
			if call == "get_l0" {
				current, ok, err := w.events.CurrentArtifact(t.Context(), "projects", artifact)
				if err != nil || !ok {
					t.Fatalf("current: %v %v", ok, err)
				}
				hidden = current.ID
				missing = connector.EventID("projects", "absent")
			}
			h, m := w.both(t, caller, call, map[string]string{"id": hidden})
			mh, mm := w.both(t, caller, call, map[string]string{"id": missing})
			mh.body = strings.ReplaceAll(mh.body, missing, hidden)
			mm.body = strings.ReplaceAll(mm.body, missing, hidden)
			if h != mh || m != mm {
				t.Fatalf("%s hidden HTTP %+v MCP %+v; absent HTTP %+v MCP %+v", call, h, m, mh, mm)
			}
		}
	}
}

func TestPostedWorkProjectInheritancePropagationAndDeletion(t *testing.T) {
	w := newPostedWork(t)
	for _, request := range []string{workProject, workStream, workUpdate} {
		w.postWork(t, request)
	}
	project := w.current(t, "retry")
	before := map[string]connector.Event{}
	for _, a := range []string{"retry#policy", "retry#policy:update:decision"} {
		child := w.current(t, a)
		before[a] = child
		if !child.ACL.Equal(project.ACL) {
			t.Fatalf("%s did not inherit project ACL", a)
		}
	}
	w.placeWork(t)
	for _, a := range []string{"retry", "retry#policy"} {
		w.distillWork(t, a, false)
	}
	for _, tt := range []struct {
		who  string
		read bool
	}{{"kyle", true}, {"ann", false}, {"outside", false}} {
		for _, a := range []string{"retry", "retry#policy"} {
			w.documentAccess(t, tt.who, a, tt.read)
		}
		w.resolves(t, api.Caller{Principal: tt.who}, "Retry policy", w.streamEntity, tt.read)
		if !tt.read {
			w.hiddenScope(t, api.Caller{Principal: tt.who}, w.streamEntity)
		}
	}
	entity, err := w.graph.Entity(t.Context(), w.projectEntity)
	if err != nil || !slices.Equal(entity.PartOf, []string{"code:acme/api"}) {
		t.Fatalf("project placement: %+v %v", entity, err)
	}
	// Changing repos re-emits all children before the successful response. Ann
	// and Kyle can still reach the project through API, but neither reads other.
	changed := strings.Replace(workProject, `"project":"acme/api"`, `"project":"acme/api"},{"source":"github","project":"acme/other"`, 1)
	changed = strings.Replace(changed, `"created_at":`, `"updated_at":"2026-10-02T12:00:00Z","created_at":`, 1)
	w.postWork(t, changed)
	project = w.current(t, "retry")
	for artifact, old := range before {
		child := w.current(t, artifact)
		if child.ID == old.ID || !child.ACL.Equal(project.ACL) || child.Payload.Text != old.Payload.Text {
			t.Fatalf("propagated %s = %+v", artifact, child)
		}
	}
	w.placeWork(t)
	for _, a := range []string{"retry", "retry#policy"} {
		w.distillWork(t, a, false)
		w.documentAccess(t, "kyle", a, false)
	}
	w.resolves(t, api.Caller{Principal: "kyle"}, "Retry policy", w.streamEntity, false)
	w.hiddenScope(t, api.Caller{Principal: "kyle"}, w.projectEntity)
	w.hiddenScope(t, api.Caller{Principal: "kyle"}, w.streamEntity)
	count := func() int {
		t.Helper()
		all, err := w.events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: "projects"}, Limit: l0.MaxLimit})
		if err != nil {
			t.Fatal(err)
		}
		return len(all)
	}
	n := count()
	w.postWork(t, changed)
	if count() != n {
		t.Fatal("identical project repost wrote events")
	}
	w.postWork(t, `{"kind":"project","project":"retry","deleted":true}`)
	if _, ok, err := w.events.CurrentArtifact(t.Context(), "projects", "retry"); err != nil || ok {
		t.Fatalf("project still current: %v %v", ok, err)
	}
	all, err := w.events.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: "projects", Kind: connector.KindTombstone}, Limit: 10})
	if err != nil || len(all) != 1 || all[0].Payload.Target != "retry" {
		t.Fatalf("tombstone: %+v %v", all, err)
	}
	w.placeWork(t)
	if _, err := w.graph.Entity(t.Context(), w.projectEntity); !errors.Is(err, l2.ErrNotFound) {
		t.Fatalf("deleted entity: %v", err)
	}
	stream, err := w.graph.Entity(t.Context(), w.streamEntity)
	if err != nil || len(stream.PartOf) != 0 {
		t.Fatalf("orphaned child: %+v %v", stream, err)
	}
	_ = w.current(t, "retry#policy:update:decision")
}

func TestPostedWorkAttributedUpdateStandingAndScopeRecovery(t *testing.T) {
	for _, tt := range []struct {
		name, author string
	}{{"attributed", "kyle"}, {"unattributed", ""}} {
		t.Run(tt.name, func(t *testing.T) {
			w := newPostedWork(t)
			w.postWork(t, workProject)
			w.postWork(t, workStream)
			update := workUpdate
			if tt.author != "" {
				update = strings.TrimSuffix(update, "}") + `,"author":"` + tt.author + `"}`
			}
			// The courier did not author the root, so its strongest role
			// in the attributed document is agent (L1 retains one role).
			posterID := "factory"
			if tt.author != "" {
				posterID = "courier"
			}
			w.postWorkAs(t, posterID, update)
			event := w.current(t, "retry#policy:update:decision")
			wantAuthor := connector.Identity{Source: "projects", Kind: connector.IdentityAgent, NativeID: "factory"}
			if tt.author != "" {
				wantAuthor.Kind = connector.IdentityUser
				wantAuthor.NativeID = tt.author
			}
			if event.Payload.Author == nil || *event.Payload.Author != wantAuthor {
				t.Fatalf("update author = %+v, want %+v", event.Payload.Author, wantAuthor)
			}
			resolver, err := w.cfg.Repo.Resolver()
			if err != nil {
				t.Fatal(err)
			}
			if got := resolver.Resolve(wantAuthor); got.Status != principal.Resolved || got.Principal.ID != wantAuthor.NativeID {
				t.Fatalf("update author resolution = %+v", got)
			}
			w.placeWork(t)
			doc := w.distillWork(t, "retry#policy", true)
			for _, read := range []struct{ call, id string }{{"get_l0", event.ID}, {"get_l1", doc.ID}} {
				h, m := w.both(t, api.Caller{Principal: "kyle"}, read.call, map[string]string{"id": read.id})
				if h.status != http.StatusOK || m.isError || h.body != m.body {
					t.Fatalf("%s: HTTP %+v MCP %+v", read.call, h, m)
				}
				if read.call == "get_l0" {
					if err := json.Unmarshal([]byte(h.body), &event); err != nil {
						t.Fatal(err)
					}
					if event.Payload.Author == nil || *event.Payload.Author != wantAuthor {
						t.Fatalf("served update author = %+v, want %+v", event.Payload.Author, wantAuthor)
					}
				} else if err := json.Unmarshal([]byte(h.body), &doc); err != nil {
					t.Fatal(err)
				}
			}
			if tt.author != "" {
				poster := connector.Participant{Identity: connector.Identity{Source: "projects", Kind: connector.IdentityAgent, NativeID: posterID}, Role: connector.RoleAgent}
				if !slices.Contains(event.Payload.Participants, poster) {
					t.Fatalf("update participants = %+v; missing poster as agent", event.Payload.Participants)
				}
				for _, want := range []l1.Participant{{PrincipalID: "kyle", Role: connector.RoleAuthor}, {PrincipalID: posterID, Role: connector.RoleAgent}} {
					if !slices.Contains(doc.Participants, want) {
						t.Fatalf("document participants = %+v; missing %+v", doc.Participants, want)
					}
				}
			}
			for _, phase := range []string{"distillation", "startup recovery"} {
				if phase == "startup recovery" {
					if _, err := w.pool.Exec(t.Context(), `DELETE FROM queue_job WHERE kind='assert'`); err != nil {
						t.Fatal(err)
					}
					if _, err := assertworker.Sweep(t.Context(), w.pool, w.cfg.Repo); err != nil {
						t.Fatal(err)
					}
				}
				var key string
				if err := w.pool.QueryRow(t.Context(), `SELECT serial_key FROM queue_job WHERE kind='assert' AND target_id=$1`, doc.ID).Scan(&key); err != nil || key != "api" {
					t.Fatalf("%s scope=%q %v", phase, key, err)
				}
			}
			budget, _ := w.cfg.Repo.LLM.Tier(llm.TierAssert)
			fixture := llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(doc, nil, budget.MaxTokens), Response: llm.Response{JSON: json.RawMessage(`{"assertions":[{"topic":"new","topic_name":"How many times delivery retries","position":"Retry twice, then stop."}]}`), StopReason: llm.StopEnd}}
			a, err := assertworker.New(w.pool, workRegistry(t, "assert-retry", []llm.CompletionFixture{fixture}), &w.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := a.Assert(t.Context(), doc.ID, "api"); err != nil || result.StancesWritten != 1 {
				t.Fatalf("assert: %+v %v", result, err)
			}
			topics, err := w.graph.Topics(t.Context(), "api")
			if err != nil || len(topics) != 1 {
				t.Fatalf("topics: %+v %v", topics, err)
			}
			standing := func(want l2.Tier) {
				t.Helper()
				h, m := w.both(t, api.Caller{Principal: "kyle"}, "stance_history", map[string]string{"topic": topics[0].ID})
				if h.status != http.StatusOK || m.isError || h.body != m.body {
					t.Fatalf("standing want %s; HTTP %+v MCP %+v", want, h, m)
				}
				var history api.History
				if err := json.Unmarshal([]byte(h.body), &history); err != nil {
					t.Fatal(err)
				}
				if len(history.Stances) != 1 || history.Current != history.Stances[0].ID || history.Tier != string(want) {
					t.Fatalf("standing = %+v, want one current stance at tier %s", history, want)
				}
			}
			// Attribution changes the event and document participants. It does
			// not grant authority to the stance drawn from the whole document.
			standing(l2.TierInferred)
			held, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			gesture, recorded, err := l2.RecordGesture(held, w.pool, w.cfg.Repo, l2.GestureRequest{
				Event: connector.EventID("hearsay", "work-ratify"), Principal: "kyle",
				Action: l2.GestureRatify, Documents: []string{doc.ID},
			})
			if err != nil || !recorded || gesture.Principal != "kyle" || gesture.Scope != "api" || len(gesture.Stances) != 1 {
				t.Fatalf("ratify: %+v, recorded=%v, %v", gesture, recorded, err)
			}
			standing(l2.TierRatified)
		})
	}
}
