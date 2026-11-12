//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// tiers is a model registry for tests of the work records. Distilling a
// document (a spec) calls the `distill` tier, and this answers it with the
// document's own first line, so that what a read serves can be told from the
// text that was posted; a document whose text says "We decide ..." concluded
// that, and the `assert` tier reads it as a position on a topic of its own.
// Placing a decision calls no tier at all, which a run with fail set — every
// completion refused, and counted — shows.
type tiers struct {
	fail  atomic.Bool
	calls atomic.Int64
}

func (r *tiers) Completer(tier llm.Tier) (llm.Completer, error) { return tierOf{r, tier}, nil }
func (r *tiers) Embedder() (llm.Embedder, error)                { return nil, llm.ErrTierNotConfigured }
func (r *tiers) Usage() []llm.Usage                             { return nil }

type tierOf struct {
	*tiers
	tier llm.Tier
}

func (r tierOf) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	r.calls.Add(1)
	if r.fail.Load() {
		return llm.Response{}, errors.New("the model is down")
	}
	text := strings.TrimSpace(req.Messages[0].Text)
	first, _, _ := strings.Cut(text, "\n")
	decided := ""
	if i := strings.Index(text, "We decide"); i >= 0 {
		decided, _, _ = strings.Cut(text[i:], "\n")
	}
	var answer any
	switch r.tier {
	case llm.TierAssert:
		answer = map[string]any{"assertions": []map[string]any{{"topic": "new", "topic_name": "Spec: " + decided, "position": decided}}}
	default:
		answer = map[string]any{"summary": first, "outcome_kind": "none"}
		if decided != "" {
			answer = map[string]any{"summary": first, "outcome": decided, "outcome_kind": "decided"}
		}
	}
	raw, _ := json.Marshal(answer)
	return llm.Response{JSON: raw, StopReason: llm.StopEnd}, nil
}

// The policy docs/config.md documents for an owner/work pair, read from the
// documentation itself so that what is tested is what is written.
func documentedPair(t *testing.T) config.Repo {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "config.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)<!-- example: work-authority/hearsay.yaml -->\n```yaml\n(.*?)```").FindSubmatch(raw)
	if m == nil {
		t.Fatal("docs/config.md has no work-authority example")
	}
	file := filepath.Join(t.TempDir(), "hearsay.yaml")
	if err := os.WriteFile(file, m[1], 0o600); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"HEARSAY_OSMIA_TOKEN"} {
		t.Setenv(env, "work-token-osmia")
	}
	repo, err := config.Load(file)
	if err != nil {
		t.Fatalf("the documented pair does not load: %v", err)
	}
	return repo
}

// pair builds the world's configuration from a pair's: the documented sources,
// the people who read, and GitHub for the repositories a project may declare.
func pair(t *testing.T, repo config.Repo) config.Repo {
	t.Helper()
	repo.Sources = append(repo.Sources, connector.SourceConfig{ID: "github", Type: "github", Containers: []string{"acme/api", "acme/other"}})
	people := []principal.Principal{
		{ID: "ann", Kind: principal.KindHuman, TokenEnv: "HEARSAY_DECISION_ANN", Grant: principal.Grant{Scopes: principal.AllScopes()}},
		{ID: "outsider", Kind: principal.KindHuman, TokenEnv: "HEARSAY_DECISION_OUTSIDER", Grant: principal.Grant{Scopes: principal.AllScopes()}},
		{ID: "api-team", Kind: principal.KindTeam, Identities: []principal.Identity{{Source: "github", NativeID: "acme/api"}}, Members: []string{"ann"}},
		{ID: "other-team", Kind: principal.KindTeam, Identities: []principal.Identity{{Source: "github", NativeID: "acme/other"}}, Members: []string{"outsider"}},
	}
	for i := range repo.Principals {
		p := &repo.Principals[i]
		if p.ID == "kyle" {
			p.TokenEnv, p.Grant = "HEARSAY_DECISION_KYLE", principal.Grant{Scopes: principal.AllScopes()}
		}
	}
	repo.Principals = append(repo.Principals, people...)
	for _, p := range repo.Principals {
		if p.TokenEnv != "" && p.ID != "osmia" {
			t.Setenv(p.TokenEnv, "token-"+p.ID)
		}
	}
	return repo
}

type decisionWorld struct {
	t         *testing.T
	pool      *pgxpool.Pool
	repo      config.Repo
	cfg       config.Config
	events    *l0.Store
	models    *tiers
	hooks     map[string]http.Handler
	server    *httptest.Server
	distiller *distiller.Distiller
	asserter  *assertworker.Asserter
	pump      *distiller.Pump
	distills  *queue.Client
	asserts   *queue.Client
	clock     int
}

func newDecisionWorld(t *testing.T, repo config.Repo, models *tiers) *decisionWorld {
	t.Helper()
	pool, err := db.Connect(t.Context(), api.ScratchDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg := config.Default()
	cfg.Repo = repo
	cfg.Repo.LLM = llm.Default()
	w := &decisionWorld{t: t, pool: pool, repo: cfg.Repo, cfg: cfg, events: l0.New(pool), models: models, hooks: map[string]http.Handler{}}

	var ids []string
	for _, s := range cfg.Repo.Sources {
		ids = append(ids, s.ID)
	}
	factory := work.NewFactory(cfg.Repo.Principals, ids, nil, work.WithSources(cfg.Repo.Sources))
	for _, src := range cfg.Repo.Sources {
		if src.Type != work.Type {
			continue
		}
		c, err := factory(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		wc := c.(*work.Connector)
		w.hooks[src.ID] = wc.Handler(connector.NewGate(w.events, src.ID, wc.Describe(), connector.NewAllowlist(cfg.Repo.Sources...)))
	}
	if w.distiller, err = distiller.New(pool, models, &cfg); err != nil {
		t.Fatal(err)
	}
	if w.asserter, err = assertworker.New(pool, models, &cfg); err != nil {
		t.Fatal(err)
	}
	w.pump = distiller.NewPump(pool, distiller.PumpOptions{})
	if w.distills, err = queue.New(pool, queue.Config{Kind: distiller.JobKind()}); err != nil {
		t.Fatal(err)
	}
	if w.asserts, err = queue.New(pool, queue.Config{Kind: l2.AssertKind()}); err != nil {
		t.Fatal(err)
	}
	calls, err := api.NewCalls(pool, cfg.Repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.server = httptest.NewServer(api.Handler(calls, pool))
	t.Cleanup(w.server.Close)
	return w
}

// repositories are what a project may declare, each readable by its team.
func (w *decisionWorld) repositories() {
	w.t.Helper()
	for _, repo := range []string{"acme/api", "acme/other"} {
		ev := connector.NewFake(connector.SourceConfig{ID: "github"}).NewEvent(connector.KindIssue, repo+"#1", "Repository content")
		ev.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: repo}
		ev.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: "github", NativeID: repo}}
		if _, err := w.events.Append(w.t.Context(), ev); err != nil {
			w.t.Fatal(err)
		}
	}
}

// post sends one request to a work source as the publisher.
func (w *decisionWorld) post(source, body string) (int, string) {
	w.t.Helper()
	req := httptest.NewRequestWithContext(w.t.Context(), http.MethodPost, "/hooks/"+source, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer work-token-osmia")
	rec := httptest.NewRecorder()
	w.hooks[source].ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (w *decisionWorld) must(source, body string) {
	w.t.Helper()
	if code, resp := w.post(source, body); code != http.StatusAccepted && code != http.StatusNoContent {
		w.t.Fatalf("POST /hooks/%s %s = %d %s", source, body, code, resp)
	}
}

// at is an RFC 3339 time n minutes into the run, so every revision is later
// than the one before it.
func at(n int) string {
	return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute).Format(time.RFC3339)
}

// feedReady waits until the change feed can show every event written so far.
// It shows an event once its transaction is older than every one open in the
// cluster, so another test's open transaction holds it back.
func (w *decisionWorld) feedReady() {
	w.t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var ready bool
		if err := w.pool.QueryRow(w.t.Context(), `SELECT coalesce(max(xact_id) < pg_snapshot_xmin(pg_current_snapshot()), true) FROM l0_events`).Scan(&ready); err != nil {
			w.t.Fatal(err)
		}
		if ready {
			return
		}
	}
	w.t.Fatal("the change feed did not catch up")
}

func (w *decisionWorld) tick() string { w.clock++; return at(w.clock) }

func rec(kind string, fields map[string]any) string {
	fields["kind"] = kind
	raw, _ := json.Marshal(fields)
	return string(raw)
}

// project posts the project; repos are GitHub repositories it declares.
func (w *decisionWorld) project(repos ...string) {
	w.t.Helper()
	fields := map[string]any{"project": "p1", "name": "Project one", "description": "A project.", "author": "kyle", "created_at": at(0), "updated_at": w.tick()}
	if len(repos) > 0 {
		var refs []map[string]string
		for _, r := range repos {
			refs = append(refs, map[string]string{"source": "github", "project": r})
		}
		fields["repos"] = refs
	}
	w.must("projects", rec("project", fields))
}

func (w *decisionWorld) stream(id string) {
	w.t.Helper()
	w.must("projects", rec("workstream", map[string]any{"project": "p1", "workstream": id, "name": "Stream " + id, "created_at": at(0)}))
}

// decision posts a decision on a source. fields add to or override the defaults.
func (w *decisionWorld) decision(source, id string, fields map[string]any) {
	w.t.Helper()
	f := map[string]any{"project": "p1", "decision": id, "outcome": "Outcome of " + id, "decider": "chief_of_staff", "created_at": at(0), "updated_at": w.tick()}
	for k, v := range fields {
		f[k] = v
	}
	w.must(source, rec("decision", f))
}

func (w *decisionWorld) document(source, id, body string, fields map[string]any) {
	w.t.Helper()
	f := map[string]any{"project": "p1", "document": id, "name": "Doc " + id, "body": body, "created_at": at(0), "updated_at": w.tick()}
	for k, v := range fields {
		f[k] = v
	}
	w.must(source, rec("document", f))
}

func (w *decisionWorld) remove(source, kind, id string, workstream string) {
	w.t.Helper()
	f := map[string]any{"project": "p1", kind: id, "deleted": true}
	if workstream != "" {
		f["workstream"] = workstream
	}
	w.must(source, rec(kind, f))
}

// settle runs the distiller and the assertion worker over everything that is
// waiting, the way the services would, until nothing is.
func (w *decisionWorld) settle() {
	w.t.Helper()
	ctx := w.t.Context()
	quiet := 0
	for round := 0; round < 400; round++ {
		w.feedReady()
		moved := false
		for {
			p, err := w.pump.Once(ctx)
			if err != nil {
				w.t.Fatal(err)
			}
			moved = moved || p.Events > 0
			if p.Events < w.pump.Batch() {
				break
			}
		}
		for _, lane := range []struct {
			q      *queue.Client
			handle func(context.Context, queue.Job) error
		}{{w.distills, w.distiller.Handle}, {w.asserts, w.asserter.Handle}} {
			for {
				jobs, err := lane.q.Claim(ctx)
				if err != nil {
					w.t.Fatal(err)
				}
				if len(jobs) == 0 {
					break
				}
				moved = true
				for _, job := range jobs {
					if err := lane.handle(ctx, job); err != nil {
						w.t.Fatalf("job %s %s: %v", job.Kind.Name, job.TargetID, err)
					}
					if _, err := lane.q.Complete(ctx, job); err != nil {
						w.t.Fatal(err)
					}
				}
			}
		}
		if _, err := assertworker.SeedEntities(ctx, w.pool, w.repo, nil); err != nil {
			w.t.Fatal(err)
		}
		if moved {
			quiet = 0
			continue
		}
		if quiet++; quiet >= 2 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	w.t.Fatal("the work did not settle")
}

func (w *decisionWorld) call(who, call string, args any) (int, []byte) {
	w.t.Helper()
	body, _ := json.Marshal(args)
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodPost, w.server.URL+"/v1/"+call, bytes.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set(api.PrincipalHeader, who)
	req.Header.Set(api.AuthorizationHeader, "Bearer token-"+who)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, got
}

func (w *decisionWorld) bundle(who, scope string) bundle.Bundle {
	w.t.Helper()
	code, raw := w.call(who, "get_bundle", map[string]string{"scope": scope})
	if code != http.StatusOK {
		w.t.Fatalf("get_bundle(%s) as %s = %d %s", scope, who, code, raw)
	}
	var b bundle.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		w.t.Fatal(err)
	}
	return b
}

func (w *decisionWorld) traverse(who, scope string) l3.Subgraph {
	w.t.Helper()
	code, raw := w.call(who, "traverse", map[string]any{
		"start": []map[string]string{{"type": "entity", "id": scope}}, "steps": []map[string]string{{"edge": "topics"}},
	})
	if code != http.StatusOK {
		w.t.Fatalf("traverse(%s) as %s = %d %s", scope, who, code, raw)
	}
	var g l3.Subgraph
	if err := json.Unmarshal(raw, &g); err != nil {
		w.t.Fatal(err)
	}
	return g
}

// entry is what a read serves of one decision.
type entry struct {
	Status, SupersededBy, Tier, Outcome, Decider string
}

// log is the decisions a bundle serves, by decision id, and fails the test on
// one served twice. A bundle and a traversal of the same entity serve the same
// log.
func (w *decisionWorld) log(who, scope string) map[string]entry {
	w.t.Helper()
	out := map[string]entry{}
	for _, s := range w.bundle(who, scope).Stances {
		if s.Decision == nil {
			continue
		}
		if _, dup := out[s.Decision.ID]; dup {
			w.t.Fatalf("decision %s is served twice by get_bundle(%s)", s.Decision.ID, scope)
		}
		out[s.Decision.ID] = entry{s.Decision.Status, s.Decision.SupersededBy, s.Tier, s.Current, s.Decision.Decider}
	}
	traversed := map[string]entry{}
	for _, n := range w.traverse(who, scope).Nodes {
		if n.Decision == nil || n.Tier == "" {
			continue
		}
		if _, dup := traversed[n.Decision.ID]; dup {
			w.t.Fatalf("decision %s is served twice by traverse(%s)", n.Decision.ID, scope)
		}
		traversed[n.Decision.ID] = entry{n.Decision.Status, n.Decision.SupersededBy, n.Tier, fmt.Sprint(n.Current), n.Decision.Decider}
	}
	// A bundle holds only what fits its budget, and these logs are small.
	if fmt.Sprint(out) != fmt.Sprint(traversed) {
		w.t.Fatalf("get_bundle(%s) serves %v but traverse serves %v", scope, out, traversed)
	}
	return out
}

func (w *decisionWorld) wantLog(who, scope string, want map[string]entry) {
	w.t.Helper()
	got := w.log(who, scope)
	for id, e := range want {
		g, ok := got[id]
		if !ok || g.Status != e.Status || g.SupersededBy != e.SupersededBy || g.Tier != e.Tier || (e.Outcome != "" && g.Outcome != e.Outcome) {
			w.t.Errorf("%s serves decision %s as %+v, want %+v", scope, id, g, e)
		}
	}
	for id, g := range got {
		if _, ok := want[id]; !ok {
			w.t.Errorf("%s serves decision %s (%+v), want it absent", scope, id, g)
		}
	}
}

const (
	proj = "work:projects:p1"
	w1   = "work:projects:p1#w1"
	w2   = "work:projects:p1#w2"
)

func current(by string) entry     { return entry{Status: l2.DecisionCurrent, SupersededBy: by} }
func (e entry) in(t string) entry { e.Tier = t; return e }
func superseded(by string) entry  { return entry{Status: l2.DecisionSuperseded, SupersededBy: by} }

// Documents of both sources are specs about the work source's entity; a
// revision is what the bundle then serves; decisions are the entity's log with
// outcome and decider, and the tier follows the source the record was posted to
// and nothing else: not its author, nor its decider.
func TestDecisionLogTiersFollowTheSource(t *testing.T) {
	w := newDecisionWorld(t, pair(t, documentedPair(t)), &tiers{})
	w.project()
	w.stream("w1")
	w.document("projects-owner", "charter", "We decide to freeze scope.", map[string]any{"author": "kyle"})
	w.document("projects", "notes", "We decide to move fast.", map[string]any{"author": "kyle"})
	w.document("projects-owner", "stream-charter", "Stream charter.", map[string]any{"workstream": "w1", "author": "kyle"})
	w.decision("projects-owner", "ruling", map[string]any{"workstream": "w1", "decider": "owner", "author": "kyle", "outcome": "Retry twice.", "context": "Ctx", "rationale": "Why", "rejected_options": []string{"never"}, "spec_document": "charter", "question": "q-1"})
	w.decision("projects", "assumption", map[string]any{"workstream": "w1", "decider": "chief_of_staff", "outcome": "Assume five."})
	w.decision("projects", "relay", map[string]any{"workstream": "w1", "decider": "owner", "author": "kyle", "outcome": "Relayed ruling."})
	w.settle()

	b := w.bundle("kyle", w1)
	byTopic := map[string]bundle.Stance{}
	for _, s := range b.Stances {
		if s.Decision != nil {
			byTopic[s.Decision.ID] = s
		}
	}
	for id, tier := range map[string]string{"ruling": "ratified", "assumption": "inferred", "relay": "inferred"} {
		s, ok := byTopic[id]
		if !ok || s.Tier != tier {
			t.Errorf("decision %s: served %+v, want tier %s", id, s, tier)
		}
	}
	if s := byTopic["ruling"]; s.Current != "Retry twice." || s.Decision == nil || s.Decision.Decider != "owner" || s.Decision.Status != l2.DecisionCurrent {
		t.Errorf("ruling served as %+v, want its outcome, decider and status", s)
	}
	// The full record is the decision's document: context, rationale, rejected
	// options and links.
	code, raw := w.call("kyle", "get_l1", map[string]string{"id": byTopic["ruling"].Decision.L1})
	if code != http.StatusOK {
		t.Fatalf("get_l1 = %d %s", code, raw)
	}
	var full l1.Stored
	if err := json.Unmarshal(raw, &full); err != nil {
		t.Fatal(err)
	}
	d := full.Body.Decision
	if full.Kind != l1.KindDecision || full.ArtifactClass != config.ArtifactDecision || d == nil || d.Context != "Ctx" || d.Rationale != "Why" ||
		!slices.Equal(d.RejectedOptions, []string{"never"}) || d.SpecDocument != "charter" || d.Question != "q-1" || d.WorkSource != "projects" || full.Source.System != "projects-owner" {
		t.Errorf("decision document = %+v %+v, want the structured record on the owner source", full.Document, d)
	}

	// Both sources' documents are specs about the work source's entities, and the
	// owner's is about the work source's, not one named after the owner source.
	for scope, lines := range map[string][]string{proj: {"We decide to freeze scope.", "We decide to move fast."}, w1: {"Stream charter."}} {
		seen := map[string]string{}
		for _, a := range w.bundle("kyle", scope).Anchors {
			seen[a.Line] = a.Kind
		}
		for _, line := range lines {
			if seen[line] != "wiki_section" {
				t.Errorf("anchors of %s = %v, want %q as a section", scope, seen, line)
			}
		}
	}
	for _, id := range []string{"projects-owner", "projects"} {
		docs, err := l1.New(w.pool).List(t.Context(), l1.ListOptions{Source: id, Class: config.ArtifactSpec})
		if err != nil || len(docs) == 0 || slices.ContainsFunc(docs, func(d l1.Stored) bool {
			return !slices.Contains(d.Scope, proj) || slices.ContainsFunc(d.Scope, func(s string) bool { return strings.Contains(s, "projects-owner") })
		}) {
			t.Errorf("spec documents of %s = %+v, %v, want them about %s and no entity named after the owner source", id, docs, err, proj)
		}
	}
	// What a document concludes is a stance whose tier follows the source it was
	// posted to: the owner's ratifies, and the work source's — even with the
	// owner as its author — stays inferred.
	if got := w.specStances(proj); got["Spec: We decide to freeze scope."] != "ratified" || got["Spec: We decide to move fast."] != "inferred" {
		t.Errorf("document stances of %s = %v, want the owner source's ratified and the work source's inferred", proj, got)
	}
	// A revision replaces what the document concluded, and its tier stays the source's.
	defer func() {
		if got := w.specStances(proj); got["Spec: We decide to freeze scope and ship often."] != "ratified" || got["Spec: We decide to move fast."] != "inferred" {
			t.Errorf("document stances after a revision = %v", got)
		}
	}()

	// A revision of a document is what the bundle serves.
	w.document("projects-owner", "charter", "We decide to freeze scope and ship often.", map[string]any{"author": "kyle"})
	w.settle()
	found := false
	for _, a := range w.bundle("kyle", proj).Anchors {
		found = found || a.Line == "We decide to freeze scope and ship often."
		if a.Line == "We decide to freeze scope." {
			t.Errorf("the superseded text is still served: %+v", a)
		}
	}
	if !found {
		t.Errorf("anchors = %+v, want the revised text", w.bundle("kyle", proj).Anchors)
	}
}

// specStances are the stances read from documents on an entity, by topic name,
// with the tier each is served at.
func (w *decisionWorld) specStances(scope string) map[string]string {
	out := map[string]string{}
	for _, s := range w.bundle("kyle", scope).Stances {
		if s.Decision == nil {
			out[s.Topic] = s.Tier
		}
	}
	return out
}

// Under the built-in policy nothing a work source records ratifies on its own:
// neither source's documents nor decisions, whoever decided.
func TestDecisionsDoNotRatifyUnderTheDefaultPolicy(t *testing.T) {
	repo := pair(t, documentedPair(t))
	repo.Authority = config.Authority{}
	w := newDecisionWorld(t, repo, &tiers{})
	w.project()
	w.document("projects-owner", "freeze", "We decide to freeze scope.", map[string]any{"author": "kyle"})
	w.document("projects", "fast", "We decide to move fast.", map[string]any{"author": "kyle"})
	w.decision("projects-owner", "ruling", map[string]any{"decider": "owner", "author": "kyle"})
	w.decision("projects", "assumption", nil)
	w.settle()
	w.wantLog("kyle", proj, map[string]entry{"ruling": current("").in("inferred"), "assumption": current("").in("inferred")})
	if got := w.specStances(proj); got["Spec: We decide to freeze scope."] != "inferred" || got["Spec: We decide to move fast."] != "inferred" {
		t.Errorf("document stances = %v, want both inferred under the default policy", got)
	}
}

// Supersession follows the latest revisions of the held decisions, across both
// sources and across workstreams, and the log serves each decision once, on the
// entity it belongs to. A workstream also serves the decisions of its project,
// which it inherits.
func TestDecisionSupersessionFollowsLatestRevisions(t *testing.T) {
	w := newDecisionWorld(t, pair(t, documentedPair(t)), &tiers{})
	w.project()
	w.stream("w1")
	w.stream("w2")
	ratified, inferred := "ratified", "inferred"
	none := map[string]entry{}

	// A is the work source's, on w1; B the owner source's, on w2.
	w.decision("projects", "A", map[string]any{"workstream": "w1", "outcome": "First A."})
	w.decision("projects-owner", "B", map[string]any{"workstream": "w2", "decider": "owner", "supersedes": "A", "author": "kyle", "outcome": "B."})
	w.settle()
	w.wantLog("kyle", w1, map[string]entry{"A": superseded("B").in(inferred)})
	w.wantLog("kyle", w2, map[string]entry{"B": current("").in(ratified)})
	w.wantLog("kyle", proj, none)

	// A is revised: served at its new revision, still superseded, once.
	w.decision("projects", "A", map[string]any{"workstream": "w1", "outcome": "Second A."})
	w.settle()
	w.wantLog("kyle", w1, map[string]entry{"A": {Status: l2.DecisionSuperseded, SupersededBy: "B", Tier: inferred, Outcome: "Second A."}})

	// B is revised to drop its supersession: A is current again.
	w.decision("projects-owner", "B", map[string]any{"workstream": "w2", "decider": "owner", "author": "kyle", "outcome": "B."})
	w.settle()
	w.wantLog("kyle", w1, map[string]entry{"A": current("").in(inferred)})
	w.wantLog("kyle", w2, map[string]entry{"B": current("").in(ratified)})

	// B is revised to supersede C, which is the project's: C is superseded and A
	// is not. C is served on the project, and on each workstream that inherits it.
	w.decision("projects", "C", map[string]any{"outcome": "C."})
	w.decision("projects-owner", "B", map[string]any{"workstream": "w2", "decider": "owner", "author": "kyle", "outcome": "B.", "supersedes": "C"})
	w.settle()
	w.wantLog("kyle", w1, map[string]entry{"A": current("").in(inferred), "C": superseded("B").in(inferred)})
	w.wantLog("kyle", w2, map[string]entry{"B": current("").in(ratified), "C": superseded("B").in(inferred)})
	w.wantLog("kyle", proj, map[string]entry{"C": superseded("B").in(inferred)})

	// B is deleted: C is current, and B is gone.
	w.remove("projects-owner", "decision", "B", "w2")
	w.settle()
	w.wantLog("kyle", w1, map[string]entry{"A": current("").in(inferred), "C": current("").in(inferred)})
	w.wantLog("kyle", w2, map[string]entry{"C": current("").in(inferred)})
	w.wantLog("kyle", proj, map[string]entry{"C": current("").in(inferred)})

	// A superseded decision that is deleted leaves its successor current, and
	// when it is posted again it is superseded again.
	w.decision("projects", "D", map[string]any{"workstream": "w2", "supersedes": "C", "outcome": "D."})
	w.settle()
	w.wantLog("kyle", proj, map[string]entry{"C": superseded("D").in(inferred)})
	w.wantLog("kyle", w2, map[string]entry{"C": superseded("D").in(inferred), "D": current("").in(inferred)})
	w.remove("projects", "decision", "C", "")
	w.settle()
	w.wantLog("kyle", w2, map[string]entry{"D": current("").in(inferred)})
	w.wantLog("kyle", proj, none)
	w.decision("projects", "C", map[string]any{"outcome": "C again."})
	w.settle()
	w.wantLog("kyle", w2, map[string]entry{"C": superseded("D").in(inferred), "D": current("").in(inferred)})
}

// A decision of a workstream superseded by one of the project, on the other
// source: the workstream serves it superseded by the project's decision, which
// it also inherits, and the project serves the decision that is its own as
// current.
func TestSupersessionAcrossLevelsAndSources(t *testing.T) {
	for _, order := range []string{"owner supersedes work", "work supersedes owner"} {
		t.Run(order, func(t *testing.T) {
			w := newDecisionWorld(t, pair(t, documentedPair(t)), &tiers{})
			w.project()
			w.stream("w1")
			first, second := "projects", "projects-owner"
			if order == "work supersedes owner" {
				first, second = second, first
			}
			w.decision(first, "A", map[string]any{"workstream": "w1", "author": "kyle"})
			w.decision(second, "B", map[string]any{"supersedes": "A", "author": "kyle"})
			w.settle()
			tier := map[string]string{"projects": "inferred", "projects-owner": "ratified"}
			w.wantLog("kyle", w1, map[string]entry{"A": superseded("B").in(tier[first]), "B": current("").in(tier[second])})
			w.wantLog("kyle", proj, map[string]entry{"B": current("").in(tier[second])})
		})
	}
}

// Nothing is contested because of a supersession.
func TestSupersededDecisionContestsNothing(t *testing.T) {
	w := newDecisionWorld(t, pair(t, documentedPair(t)), &tiers{})
	w.project()
	w.decision("projects", "A", map[string]any{"outcome": "Use A."})
	w.decision("projects-owner", "B", map[string]any{"decider": "owner", "author": "kyle", "supersedes": "A", "outcome": "Use B instead."})
	w.settle()
	for _, s := range w.bundle("kyle", proj).Stances {
		if s.Tier == string(l2.TierContested) {
			t.Errorf("%+v is contested", s)
		}
	}
}

// The log a project's decisions make does not depend on the order the two
// sources' records arrive in, on whether the records are placed one after the
// other or at once, or on a replay of the placement jobs as after a restart —
// under the default per-source keys and under covering scopes that differ
// between the sources.
func TestDecisionPlacementIsIndependentOfOrderConcurrencyAndReplay(t *testing.T) {
	scopes := map[string][]config.Scope{
		"default keys":         nil,
		"differing covering":   {{ID: "wscope", Sources: []config.ScopeSource{{Source: "projects", Containers: []string{"*"}}}}, {ID: "oscope", Sources: []config.ScopeSource{{Source: "projects-owner", Containers: []string{"*"}}}}},
		"owner scope only":     {{ID: "oscope", Sources: []config.ScopeSource{{Source: "projects-owner", Containers: []string{"*"}}}}},
		"one covering scope":   {{ID: "work", Sources: []config.ScopeSource{{Source: "projects", Containers: []string{"*"}}, {Source: "projects-owner", Containers: []string{"*"}}}}},
		"work scope the owner": {{ID: "wscope", Sources: []config.ScopeSource{{Source: "projects", Containers: []string{"*"}}}}},
	}
	post := func(w *decisionWorld, owner, work bool) {
		if work {
			w.decision("projects", "A", map[string]any{"workstream": "w1", "outcome": "A."})
		}
		if owner {
			w.decision("projects-owner", "B", map[string]any{"workstream": "w2", "decider": "owner", "author": "kyle", "supersedes": "A", "outcome": "B."})
		}
	}
	for name, sc := range scopes {
		t.Run(name, func(t *testing.T) {
			repo := pair(t, documentedPair(t))
			repo.Scopes = sc
			// The reference: one after the other.
			ref := newDecisionWorld(t, repo, &tiers{})
			ref.project()
			ref.stream("w1")
			ref.stream("w2")
			post(ref, false, true)
			post(ref, true, false)
			ref.settle()
			want := ref.snapshot()
			if len(want) != 2 || want["A"].Status != l2.DecisionSuperseded || want["B"].Status != l2.DecisionCurrent {
				t.Fatalf("reference log = %v", want)
			}
			// One key for both sources' placement jobs.
			keys := map[string]bool{}
			rows, err := ref.pool.Query(t.Context(), `SELECT DISTINCT serial_key FROM queue_job WHERE kind = $1 AND target_id LIKE 'l1:%' AND target_id LIKE '%decision%'`, l2.AssertKindName)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var k string
				_ = rows.Scan(&k)
				keys[k] = true
			}
			rows.Close()
			if len(keys) != 1 {
				t.Errorf("decision jobs ran under keys %v, want one", keys)
			}

			// The other arrival order, processing the successor first.
			rev := newDecisionWorld(t, repo, &tiers{})
			rev.project()
			rev.stream("w1")
			rev.stream("w2")
			// B names A before A is held, which intake refuses, so B arrives
			// first without the edge and is revised to carry it once A is held.
			rev.decision("projects-owner", "B", map[string]any{"workstream": "w2", "decider": "owner", "author": "kyle", "outcome": "B."})
			rev.settle()
			post(rev, false, true)
			rev.decision("projects-owner", "B", map[string]any{"workstream": "w2", "decider": "owner", "author": "kyle", "supersedes": "A", "outcome": "B."})
			rev.settle()
			if got := rev.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("successor first: log %v, want %v", got, want)
			}

			// Both records distilled before either is placed, then the placement
			// jobs run at once, in each order, and again as on restart.
			for _, order := range [][]string{{"A", "B"}, {"B", "A"}} {
				c := newDecisionWorld(t, repo, &tiers{})
				c.project()
				c.stream("w1")
				c.stream("w2")
				post(c, true, true)
				c.distillOnly()
				ids := map[string]string{}
				docs, err := l1.New(c.pool).List(t.Context(), l1.ListOptions{Kind: l1.KindDecision})
				if err != nil || len(docs) != 2 {
					t.Fatalf("decision documents = %d, %v", len(docs), err)
				}
				for _, d := range docs {
					ids[d.Body.Decision.ID] = d.ID
				}
				var wg sync.WaitGroup
				for _, id := range order {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if _, err := assertworker.PlaceDecision(t.Context(), c.pool, c.repo.Authority, ids[id], l2.DecisionKey(c.repo, *docDecision(t, c, ids[id]))); err != nil {
							t.Error(err)
						}
					}()
				}
				wg.Wait()
				if got := c.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("concurrent %v: log %v, want %v", order, got, want)
				}
				// A replay: the worker's startup sweep after the record of what
				// was placed is lost, then the jobs it enqueues.
				if _, err := c.pool.Exec(t.Context(), `DELETE FROM l2_asserted`); err != nil {
					t.Fatal(err)
				}
				swept, err := assertworker.Sweep(t.Context(), c.pool, c.repo)
				if err != nil || swept < 2 {
					t.Fatalf("sweep = %d, %v, want the decisions enqueued", swept, err)
				}
				c.settle()
				if got := c.snapshot(); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("replayed %v: log %v, want %v", order, got, want)
				}
				// Placing again writes nothing new and serves nothing twice.
				if written, err := assertworker.PlaceDecision(t.Context(), c.pool, c.repo.Authority, ids["A"], l2.DecisionKey(c.repo, *docDecision(t, c, ids["A"]))); err != nil || written {
					t.Errorf("a replayed placement wrote a stance (%v, %v)", written, err)
				}
			}
		})
	}
}

func docDecision(t *testing.T, w *decisionWorld, id string) *l1.Decision {
	t.Helper()
	doc, err := l1.New(w.pool).Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return doc.Body.Decision
}

// snapshot is the project's decision log as the owner reads it.
func (w *decisionWorld) snapshot() map[string]entry {
	out := map[string]entry{}
	for _, scope := range []string{proj, w1, w2} {
		for id, e := range w.log("kyle", scope) {
			if held, ok := out[id]; ok && held != e {
				w.t.Fatalf("decision %s is served as %+v on one entity and %+v on another", id, held, e)
			}
			out[id] = e
		}
	}
	return out
}

// distillOnly runs the distiller but not the assertion worker.
func (w *decisionWorld) distillOnly() {
	w.t.Helper()
	ctx := w.t.Context()
	quiet := 0
	for round := 0; round < 400; round++ {
		w.feedReady()
		moved := false
		for {
			p, err := w.pump.Once(ctx)
			if err != nil {
				w.t.Fatal(err)
			}
			moved = moved || p.Events > 0
			if p.Events < w.pump.Batch() {
				break
			}
		}
		jobs, err := w.distills.Claim(ctx)
		if err != nil {
			w.t.Fatal(err)
		}
		if len(jobs) == 0 && !moved {
			if quiet++; quiet >= 2 {
				return
			}
			time.Sleep(25 * time.Millisecond)
			continue
		}
		quiet = 0
		for _, job := range jobs {
			if err := w.distiller.Handle(ctx, job); err != nil {
				w.t.Fatal(err)
			}
			if _, err := w.distills.Complete(ctx, job); err != nil {
				w.t.Fatal(err)
			}
		}
		if _, err := assertworker.SeedEntities(ctx, w.pool, w.repo, nil); err != nil {
			w.t.Fatal(err)
		}
	}
}

// No model is called to place, supersede or tier a decision: with a model that
// refuses every call, the log is still produced.
func TestDecisionLogNeedsNoModel(t *testing.T) {
	models := &tiers{}
	w := newDecisionWorld(t, pair(t, documentedPair(t)), models)
	// The project and its workstream are distilled by the model, as they are
	// whatever else happens; the decisions are what must need none.
	w.project()
	w.stream("w1")
	w.settle()
	models.fail.Store(true)
	before := models.calls.Load()
	w.decision("projects", "A", map[string]any{"workstream": "w1"})
	w.decision("projects-owner", "B", map[string]any{"workstream": "w1", "decider": "owner", "author": "kyle", "supersedes": "A"})
	w.settle()
	w.wantLog("kyle", w1, map[string]entry{"A": superseded("B").in("inferred"), "B": current("").in("ratified")})
	if n := models.calls.Load() - before; n != 0 {
		t.Errorf("%d model calls were made to place decisions, all of which failed", n)
	}
	// And no decision entered the model-based pipeline.
	var n int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_stances s JOIN l2_topics t ON t.id = s.topic_id WHERE s.evidence[1] LIKE '%decision%' AND NOT t.id LIKE 'topic:%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("stray stances: %d, %v", n, err)
	}
	topics, err := l2.New(w.pool).Topics(t.Context(), l2.ScopeKey(w.repo, "projects", "p1", nil))
	if err != nil || len(topics) != 2 {
		t.Errorf("topics = %d, %v, want one for each decision", len(topics), err)
	}
}

// Records are read by the project's poster and named author alone while it
// declares no repos; by readers of its repos when it does; and by nobody who
// lost access once a change has been processed — on both sources, through
// bundles, traversals and direct fetches.
func TestDecisionAccessFollowsTheProjectsRepos(t *testing.T) {
	w := newDecisionWorld(t, pair(t, documentedPair(t)), &tiers{})
	w.repositories()
	w.project()
	w.document("projects", "d-work", "Work doc.", nil)
	w.document("projects-owner", "d-owner", "Owner doc.", map[string]any{"author": "kyle"})
	w.decision("projects", "x-work", map[string]any{"outcome": "Work decision."})
	w.decision("projects-owner", "x-owner", map[string]any{"decider": "owner", "author": "kyle", "outcome": "Owner decision."})
	w.settle()

	handles := w.handles(t)
	if flat(handles) != 10 {
		t.Fatalf("handles = %v, want two sections and two decisions with their events, and two topics", handles)
	}
	missing := w.missingAnswer(t, "ann")
	w.wantLog("kyle", proj, map[string]entry{"x-work": current("").in("inferred"), "x-owner": current("").in("ratified")})
	// Private: the named author reads them, nobody else does, and the answer
	// is the one an unknown scope gets.
	w.empty(t, "ann", proj, missing)
	w.empty(t, "outsider", proj, w.missingAnswer(t, "outsider"))
	w.nothingByHandle(t, "ann", handles)
	w.somethingByHandle(t, "kyle", handles)

	// Readable by the readers of acme/api.
	w.project("acme/api")
	w.settle()
	viaRepos := w.handles(t)
	w.wantLog("ann", proj, map[string]entry{"x-work": current("").in("inferred"), "x-owner": current("").in("ratified")})
	w.empty(t, "outsider", proj, w.missingAnswer(t, "outsider"))
	w.somethingByHandle(t, "ann", w.handles(t))
	w.nothingByHandle(t, "outsider", handles)
	// A project that declares repos hands its access to theirs: the author, who
	// reads none of them, no longer reads it.
	w.empty(t, "kyle", proj, w.missingAnswer(t, "kyle"))
	w.nothingByHandle(t, "kyle", handles)
	w.nothingByHandle(t, "kyle", w.handles(t))

	// Repos removed: ann has lost access, once the change is processed.
	w.project()
	w.settle()
	w.empty(t, "ann", proj, missing)
	w.nothingByHandle(t, "ann", handles)
	w.nothingByHandle(t, "ann", viaRepos)
	w.somethingByHandle(t, "kyle", w.handles(t))

	// Repos again, then replaced by repositories ann cannot read.
	w.project("acme/api")
	w.settle()
	w.wantLog("ann", proj, map[string]entry{"x-work": current("").in("inferred"), "x-owner": current("").in("ratified")})
	w.project("acme/other")
	w.settle()
	w.empty(t, "ann", proj, missing)
	w.nothingByHandle(t, "ann", handles)
	w.nothingByHandle(t, "ann", viaRepos)
	w.nothingByHandle(t, "ann", w.handles(t))
	w.wantLog("outsider", proj, map[string]entry{"x-work": current("").in("inferred"), "x-owner": current("").in("ratified")})
}

// handles are the handles of everything a project's records made: L1 ids of the
// sections and decisions, topics, and the L0 events behind them.
func (w *decisionWorld) handles(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{"get_l1": nil, "stance_history": nil, "get_l0": nil}
	docs, err := l1.New(w.pool).List(t.Context(), l1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.Source.System == "github" || (d.Kind != l1.KindDecision && d.Kind != l1.KindWikiSection) {
			continue
		}
		out["get_l1"] = append(out["get_l1"], d.ID)
		out["get_l0"] = append(out["get_l0"], d.L0Refs[0])
	}
	topics, err := l2.New(w.pool).Topics(t.Context(), l2.ScopeKey(w.repo, "projects", "p1", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, topic := range topics {
		out["stance_history"] = append(out["stance_history"], topic.ID)
	}
	return out
}

func flat(h map[string][]string) (n int) {
	for _, ids := range h {
		n += len(ids)
	}
	return n
}

func (w *decisionWorld) fetch(who, call, id string) int {
	key := map[string]string{"get_l1": "id", "get_l0": "id", "stance_history": "topic"}[call]
	code, _ := w.call(who, call, map[string]string{key: id})
	return code
}

func (w *decisionWorld) nothingByHandle(t *testing.T, who string, h map[string][]string) {
	t.Helper()
	for call, ids := range h {
		for _, id := range ids {
			if code := w.fetch(who, call, id); code == http.StatusOK {
				t.Errorf("%s: %s(%s) was served", who, call, id)
			}
		}
	}
}

func (w *decisionWorld) somethingByHandle(t *testing.T, who string, h map[string][]string) {
	t.Helper()
	for call, ids := range h {
		for _, id := range ids {
			if code := w.fetch(who, call, id); code != http.StatusOK {
				t.Errorf("%s: %s(%s) = %d, want it served", who, call, id, code)
			}
		}
	}
}

// missingAnswer is the bundle an unknown work entity gets, with the scope id
// left in place, for comparing with what a reader without access is served.
func (w *decisionWorld) missingAnswer(t *testing.T, who string) string {
	t.Helper()
	code, raw := w.call(who, "get_bundle", map[string]string{"scope": "work:projects:nothing-here"})
	if code != http.StatusOK {
		t.Fatalf("get_bundle(unknown) = %d %s", code, raw)
	}
	return strings.ReplaceAll(string(raw), "work:projects:nothing-here", "SCOPE")
}

func (w *decisionWorld) empty(t *testing.T, who, scope, want string) {
	t.Helper()
	code, raw := w.call(who, "get_bundle", map[string]string{"scope": scope})
	if code != http.StatusOK {
		t.Fatalf("get_bundle(%s) as %s = %d", scope, who, code)
	}
	if got := strings.ReplaceAll(string(raw), scope, "SCOPE"); got != want {
		t.Errorf("%s reads %s as %s, want what an unknown scope gets, %s", who, scope, got, want)
	}
	code, raw = w.call(who, "traverse", map[string]any{"start": []map[string]string{{"type": "entity", "id": scope}}, "steps": []map[string]string{{"edge": "topics"}}})
	if code == http.StatusOK {
		t.Errorf("%s traverses %s: %s", who, scope, raw)
	}
}

// A document of either source is distilled into sections of class `spec` about
// the work source's entity, split at its headings; a revision is what is served
// afterwards, a section it drops goes, and a deleted document is gone.
func TestWorkDocumentLifecycleOnBothSources(t *testing.T) {
	w := newDecisionWorld(t, pair(t, documentedPair(t)), &tiers{})
	w.project()
	w.stream("w1")
	sections := func(source string) map[string]string {
		docs, err := l1.New(w.pool).List(t.Context(), l1.ListOptions{Source: source, Kind: l1.KindWikiSection})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, d := range docs {
			if d.ArtifactClass != config.ArtifactSpec || !slices.Contains(d.Scope, w1) || !slices.Contains(d.Scope, proj) {
				t.Errorf("section %s = class %s scope %v, want a spec about %s and %s", d.ID, d.ArtifactClass, d.Scope, proj, w1)
			}
			out[d.Source.NativeID] = d.RawText
		}
		return out
	}
	anchors := func() []string {
		var out []string
		for _, a := range w.bundle("kyle", w1).Anchors {
			out = append(out, a.Line)
		}
		slices.Sort(out)
		return out
	}
	for _, source := range []string{"projects", "projects-owner"} {
		t.Run(source, func(t *testing.T) {
			fields := map[string]any{"workstream": "w1", "author": "kyle"}
			w.document(source, "plan", "# Goals\n\nShip the retry cap.\n\n# Risks\n\nQueues back up.", fields)
			w.settle()
			if got := sections(source); len(got) != 2 {
				t.Fatalf("sections = %v, want one per heading", got)
			}
			if got := anchors(); !slices.Contains(got, "# Goals") || !slices.Contains(got, "# Risks") {
				t.Errorf("anchors = %v, want both sections", got)
			}
			// An identical repost changes nothing; a revision is what is served.
			w.document(source, "plan", "# Goals\n\nShip the retry cap this week.", fields)
			w.settle()
			got := sections(source)
			if len(got) != 1 {
				t.Fatalf("sections = %v, want the Risks section gone", got)
			}
			for _, text := range got {
				if !strings.Contains(text, "this week") {
					t.Errorf("section = %q, want the revised text", text)
				}
			}
			if a := anchors(); slices.Contains(a, "# Risks") {
				t.Errorf("anchors = %v, want the dropped section gone", a)
			}
			// A deleted document is gone, and its id is free again.
			w.remove(source, "document", "plan", "w1")
			w.settle()
			if got := sections(source); len(got) != 0 {
				t.Errorf("sections of a deleted document = %v", got)
			}
			if a := anchors(); slices.Contains(a, "# Goals") {
				t.Errorf("anchors = %v, want the deleted document gone", a)
			}
			w.document(source, "plan", "# Again\n\nBack.", fields)
			w.settle()
			if got := sections(source); len(got) != 1 {
				t.Errorf("sections after a repost = %v, want one", got)
			}
			w.remove(source, "document", "plan", "w1")
			w.settle()
		})
	}
}
