package work_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
)

const ownerID = "owner-records"

func ownerSource() connector.SourceConfig {
	return connector.SourceConfig{ID: ownerID, Type: work.Type, Containers: []string{"*"}, Settings: json.RawMessage(`{"attributors":["factory"],"owner_of":"projects"}`)}
}

// pair is a work source and its owner source over one store, built the way the
// connectors service builds them: from one factory, so they share their locks.
type pair struct {
	store     *memoryStore
	work      *work.Connector
	owner     *work.Connector
	workH     http.Handler
	ownerH    http.Handler
	workGate  connector.Sink
	ownerGate connector.Sink
	// private makes seedRecords post the project without repos.
	private bool
}

func newPair(t *testing.T) *pair {
	t.Helper()
	configs := []connector.SourceConfig{source(), ownerSource()}
	factory := work.NewFactory(principals(), []string{sourceID, ownerID, "github", "github-other"}, lookup, work.WithSources(configs))
	build := func(src connector.SourceConfig) *work.Connector {
		c, err := factory(t.Context(), src)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*work.Connector)
	}
	p := &pair{store: &memoryStore{}, work: build(source()), owner: build(ownerSource())}
	p.workGate = connector.NewGate(p.store, sourceID, p.work.Describe(), connector.NewAllowlist(configs...))
	p.ownerGate = connector.NewGate(p.store, ownerID, p.owner.Describe(), connector.NewAllowlist(configs...))
	p.workH = p.work.Handler(p.workGate)
	p.ownerH = p.owner.Handler(p.ownerGate)
	return p
}

func post(t *testing.T, h http.Handler, token, body string) *httpResult {
	t.Helper()
	w := request(t.Context(), h, http.MethodPost, token, body)
	return &httpResult{Code: w.Code, Body: w.Body.String()}
}

type httpResult struct {
	Code int
	Body string
}

func (r *httpResult) id(t *testing.T) string {
	t.Helper()
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Body), &got); err != nil || got.ID == "" {
		t.Fatalf("response %q has no event id: %v", r.Body, err)
	}
	return got.ID
}

func (p *pair) expect(t *testing.T, h http.Handler, body string, want int) *httpResult {
	t.Helper()
	r := post(t, h, "factory-token", body)
	if r.Code != want {
		t.Fatalf("POST %s = %d %s, want %d", body, r.Code, r.Body, want)
	}
	return r
}

func (p *pair) seed(t *testing.T) {
	t.Helper()
	p.expect(t, p.workH, fixture(t, "project"), 202)
	p.expect(t, p.workH, fixture(t, "workstream"), 202)
	p.expect(t, p.workH, with(t, fixture(t, "workstream"), map[string]any{"workstream": "other-stream", "name": "Other"}), 202)
}

func (p *pair) heldIn(t *testing.T, source, artifact string) (connector.Event, bool) {
	t.Helper()
	ev, ok, err := p.store.CurrentArtifact(t.Context(), source, artifact)
	if err != nil {
		t.Fatal(err)
	}
	return ev, ok
}

func doc(t *testing.T, fields map[string]any) string {
	t.Helper()
	return with(t, fixture(t, "document"), fields)
}

func dec(t *testing.T, fields map[string]any) string {
	t.Helper()
	return with(t, fixture(t, "decision"), fields)
}

// minimal is a decision with only what is required.
func minimal(t *testing.T, id string, fields map[string]any) string {
	t.Helper()
	body := map[string]any{"kind": "decision", "project": "payments-rewrite", "decision": id, "outcome": "Ship it.", "decider": "chief_of_staff", "created_at": "2026-10-14T18:00:00Z"}
	for k, v := range fields {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDocumentRecords(t *testing.T) {
	for _, tt := range []struct {
		name     string
		artifact string
		fields   map[string]any
	}{
		{name: "on a workstream", artifact: "payments-rewrite#retry-policy:document:spec-1"},
		{name: "on the project", artifact: "payments-rewrite:document:spec-1", fields: map[string]any{"workstream": nil}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newPair(t)
			p.seed(t)
			first := p.expect(t, p.workH, doc(t, tt.fields), 202).id(t)
			n := len(p.store.history())
			if again := p.expect(t, p.workH, doc(t, tt.fields), 202).id(t); again != first || len(p.store.history()) != n {
				t.Fatalf("identical repost = %s with %d events, want %s and %d", again, len(p.store.history()), first, n)
			}
			ev, ok := p.heldIn(t, sourceID, tt.artifact)
			if !ok || ev.ID != first || ev.Kind != connector.KindDocument {
				t.Fatalf("held = %v %s (%s), want the posted document event", ok, ev.ID, ev.Kind)
			}
			if ev.Payload.Title != "Retry policy spec" || !strings.HasPrefix(ev.Payload.Text, "# Retry policy") || ev.Payload.URL == "" {
				t.Errorf("payload = %+v, want name, markdown body and url", ev.Payload)
			}
			if ev.Payload.Container.Kind != "project" || ev.Payload.Container.NativeID != "payments-rewrite" {
				t.Errorf("container = %+v, want the project", ev.Payload.Container)
			}
			if !ev.ACL.Equal(held(t, p.store, "payments-rewrite").ACL) {
				t.Errorf("document ACL %v is not the project's", ev.ACL)
			}

			revised := p.expect(t, p.workH, doc(t, mergeMaps(tt.fields, map[string]any{"body": "# Retry policy\n\nCap at 6.", "updated_at": "2026-10-14T19:00:00Z"})), 202).id(t)
			if revised == first {
				t.Fatal("changed repost kept the same event id")
			}
			if ev, _ := p.heldIn(t, sourceID, tt.artifact); ev.ID != revised || !strings.Contains(ev.Payload.Text, "Cap at 6") {
				t.Fatalf("current revision = %s %q, want the revised one", ev.ID, ev.Payload.Text)
			}

			delFields := map[string]any{"deleted": true}
			if strings.Contains(tt.artifact, "#") {
				delFields["workstream"] = "retry-policy"
			}
			del := with(t, `{"kind":"document","project":"payments-rewrite","document":"spec-1"}`, delFields)
			tomb := p.expect(t, p.workH, del, 202)
			tombstone := p.store.history()[len(p.store.history())-1]
			if tombstone.Kind != connector.KindTombstone || tombstone.Payload.Target != tt.artifact || tomb.id(t) != tombstone.ID {
				t.Fatalf("deletion emitted %+v, want a tombstone of %s", tombstone, tt.artifact)
			}
			if _, ok := p.heldIn(t, sourceID, tt.artifact); ok {
				t.Error("deleted document is still held")
			}
			p.expect(t, p.workH, del, 204)
		})
	}
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func TestDecisionRecordFields(t *testing.T) {
	p := newPair(t)
	p.seed(t)
	p.expect(t, p.workH, doc(t, nil), 202)
	p.expect(t, p.workH, minimal(t, "d-1", map[string]any{"workstream": "retry-policy"}), 202)
	id := p.expect(t, p.workH, fixture(t, "decision"), 202).id(t)
	ev, ok := p.heldIn(t, sourceID, "payments-rewrite#retry-policy:decision:d-2")
	if !ok || ev.ID != id || ev.Kind != work.KindDecision || ev.Payload.BaseKind != connector.KindDocument {
		t.Fatalf("held = %v %+v, want a work.decision event based on document", ok, ev)
	}
	held, ok := work.Decode(ev)
	if !ok {
		t.Fatal("Decode does not read the stored event")
	}
	n := held.Native()
	want := work.Native{
		Project: "payments-rewrite", Workstream: "retry-policy", Poster: "factory", WorkSource: sourceID,
		Decision: "d-2", Context: "Settlement calls fail transiently under load.", Outcome: "Cap retries at 5 with jittered backoff.",
		Rationale: "Five attempts cover the observed outage window.", RejectedOptions: []string{"Unbounded retries", "A dead-letter queue first"},
		Decider: "owner", Supersedes: "d-1", SpecDocument: "spec-1", PullRequest: "https://github.com/acme/api/pull/42", Question: "q-17",
		SupersedesAt:   &work.Location{Source: sourceID, Artifact: "payments-rewrite#retry-policy:decision:d-1"},
		SpecDocumentAt: &work.Location{Source: sourceID, Artifact: "payments-rewrite#retry-policy:document:spec-1"},
	}
	gotRaw, _ := json.Marshal(n)
	wantRaw, _ := json.Marshal(want)
	if string(gotRaw) != string(wantRaw) {
		t.Errorf("native = %s\nwant     %s", gotRaw, wantRaw)
	}
	if held.Supersedes() != "d-1" || held.SpecDocument() != "spec-1" {
		t.Errorf("Supersedes, SpecDocument = %q, %q", held.Supersedes(), held.SpecDocument())
	}
	if ev.Payload.Author == nil || ev.Payload.Author.NativeID != "kyle" || len(ev.Payload.Links) != 1 || ev.Payload.Links[0] != n.PullRequest {
		t.Errorf("author %v links %v, want kyle and the pull request", ev.Payload.Author, ev.Payload.Links)
	}
	for _, part := range []string{"Cap retries at 5", "owner", "Unbounded retries", "q-17"} {
		if !strings.Contains(ev.Payload.Text, part) {
			t.Errorf("text %q lacks %q", ev.Payload.Text, part)
		}
	}
	if ev.Payload.Title != "Cap retries at 5 with jittered backoff." {
		t.Errorf("title = %q", ev.Payload.Title)
	}
	// A repost, a revision, a deletion.
	n0 := len(p.store.history())
	p.expect(t, p.workH, fixture(t, "decision"), 202)
	if len(p.store.history()) != n0 {
		t.Error("identical decision repost wrote")
	}
	p.expect(t, p.workH, dec(t, map[string]any{"outcome": "Cap retries at 4.", "updated_at": "2026-10-14T19:00:00Z"}), 202)
	if ev, _ := p.heldIn(t, sourceID, "payments-rewrite#retry-policy:decision:d-2"); !strings.Contains(ev.Payload.Text, "at 4") {
		t.Error("revision is not the current one")
	}
	p.expect(t, p.workH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"d-2","deleted":true}`, 202)
	if _, ok := p.heldIn(t, sourceID, "payments-rewrite#retry-policy:decision:d-2"); ok {
		t.Error("deleted decision is held")
	}
	p.expect(t, p.workH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"d-2","deleted":true}`, 204)
}

func TestRecordValidation(t *testing.T) {
	p := newPair(t)
	p.seed(t)
	tests := []struct {
		name string
		body string
		want int
	}{
		{"decision without outcome", minimal(t, "d1", map[string]any{"outcome": nil}), 400},
		{"decision with a blank outcome", minimal(t, "d1", map[string]any{"outcome": "  "}), 400},
		{"decision without decider", minimal(t, "d1", map[string]any{"decider": nil}), 400},
		{"decision with another decider", minimal(t, "d1", map[string]any{"decider": "ceo"}), 400},
		{"decision with an unknown field", minimal(t, "d1", map[string]any{"priority": "high"}), 400},
		{"decision with a document field", minimal(t, "d1", map[string]any{"body": "text"}), 400},
		{"decision with an update id", minimal(t, "d1", map[string]any{"update": "u"}), 400},
		{"decision carrying a document id", minimal(t, "d1", map[string]any{"document": "x"}), 400},
		{"decision with state", minimal(t, "d1", map[string]any{"state": "open"}), 400},
		{"decision with repos", minimal(t, "d1", map[string]any{"repos": []work.Repo{{Source: "github", Project: "a/b"}}}), 400},
		{"decision without an id", minimal(t, "", nil), 400},
		{"decision with a bad id", minimal(t, "a#b", nil), 400},
		{"decision with a bad supersedes id", minimal(t, "d1", map[string]any{"supersedes": "a:b"}), 400},
		{"decision with a bad spec document id", minimal(t, "d1", map[string]any{"spec_document": "a b"}), 400},
		{"decision with a bad pull request", minimal(t, "d1", map[string]any{"pull_request": "not a url"}), 400},
		{"decision with a bad question", minimal(t, "d1", map[string]any{"question": "has space"}), 400},
		{"decision with an empty rejected option", minimal(t, "d1", map[string]any{"rejected_options": []string{""}}), 400},
		{"decision without created_at", minimal(t, "d1", map[string]any{"created_at": nil}), 400},
		{"decision superseding itself", minimal(t, "d1", map[string]any{"supersedes": "d1"}), 400},
		{"document without a name", doc(t, map[string]any{"name": nil}), 400},
		{"document without a body", doc(t, map[string]any{"body": nil}), 400},
		{"document with a bad url", doc(t, map[string]any{"url": "ftp://x"}), 400},
		{"document with a decision field", doc(t, map[string]any{"outcome": "x"}), 400},
		{"document with an unknown field", doc(t, map[string]any{"colour": "red"}), 400},
		{"document with a description", doc(t, map[string]any{"description": "x"}), 400},
		{"document without an id", doc(t, map[string]any{"document": nil}), 400},
		{"document carrying a decision id", doc(t, map[string]any{"decision": "d"}), 400},
		{"deletion carrying content", `{"kind":"decision","project":"payments-rewrite","decision":"d1","outcome":"x","deleted":true}`, 400},
		{"project carrying an outcome", with(t, fixture(t, "project"), map[string]any{"outcome": "x"}), 400},
		{"update carrying a decider", with(t, fixture(t, "update"), map[string]any{"decider": "owner"}), 400},
		{"workstream carrying a body", with(t, fixture(t, "workstream"), map[string]any{"body": "x"}), 400},
		{"decision on a missing workstream", minimal(t, "d1", map[string]any{"workstream": "nope"}), 409},
		{"decision on a missing project", minimal(t, "d1", map[string]any{"project": "nope"}), 409},
		{"document on a missing workstream", doc(t, map[string]any{"workstream": "nope"}), 409},
		{"document on a missing project", doc(t, map[string]any{"project": "nope"}), 409},
	}
	before := len(p.store.history())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p.expect(t, p.workH, tt.body, tt.want)
			if len(p.store.history()) != before {
				t.Fatalf("a refused post wrote %d events", len(p.store.history())-before)
			}
		})
	}
	// A deleted document or decision that is not held is a 204, not a 400 or 409.
	p.expect(t, p.workH, `{"kind":"decision","project":"nope","decision":"d1","deleted":true}`, 204)
}

func TestProjectWideIDs(t *testing.T) {
	type post struct {
		name      string
		handler   func(*pair) http.Handler
		workspace string // "" is project level
	}
	places := []post{
		{"the same workstream on the other source", func(p *pair) http.Handler { return p.ownerH }, "w1"},
		{"another workstream of the same source", func(p *pair) http.Handler { return p.workH }, "w2"},
		{"another workstream on the other source", func(p *pair) http.Handler { return p.ownerH }, "w2"},
		{"project level of the same source", func(p *pair) http.Handler { return p.workH }, ""},
		{"project level of the other source", func(p *pair) http.Handler { return p.ownerH }, ""},
	}
	for _, kind := range []string{"decision", "document"} {
		build := func(t *testing.T, workspace string, fields map[string]any) string {
			t.Helper()
			base := map[string]any{"workstream": workspace}
			if workspace == "" {
				base = map[string]any{"workstream": nil}
			}
			if kind == "decision" {
				return minimal(t, "d1", mergeMaps(base, fields))
			}
			return doc(t, mergeMaps(map[string]any{"document": "d1"}, mergeMaps(base, fields)))
		}
		for _, place := range places {
			t.Run(kind+" "+place.name, func(t *testing.T) {
				p := newPair(t)
				p.seed(t)
				p.expect(t, p.workH, with(t, fixture(t, "workstream"), map[string]any{"workstream": "w1", "name": "W1"}), 202)
				p.expect(t, p.workH, with(t, fixture(t, "workstream"), map[string]any{"workstream": "w2", "name": "W2"}), 202)
				first := p.expect(t, p.workH, build(t, "w1", nil), 202).id(t)
				n := len(p.store.history())
				r := p.expect(t, place.handler(p), build(t, place.workspace, nil), 409)
				for _, part := range []string{`"d1"`, "already held", "source " + sourceID, "workstream w1", "payments-rewrite#w1:" + kind + ":d1"} {
					if !strings.Contains(r.Body, part) {
						t.Errorf("409 %q does not say %q", r.Body, part)
					}
				}
				if len(p.store.history()) != n {
					t.Fatal("the refused post wrote")
				}
				// A revision of the holder is not a collision with itself.
				p.expect(t, p.workH, build(t, "w1", map[string]any{"updated_at": "2026-10-14T20:00:00Z", "created_at": "2026-10-14T17:00:00Z"}), 202)
				// A deleted id is free again.
				p.expect(t, p.workH, `{"kind":"`+kind+`","project":"payments-rewrite","workstream":"w1","`+kind+`":"d1","deleted":true}`, 202)
				if again := p.expect(t, place.handler(p), build(t, place.workspace, nil), 202).id(t); again == first {
					t.Error("reposting elsewhere reused the old event id")
				}
			})
		}
	}

	t.Run("a document and a decision may share an id", func(t *testing.T) {
		p := newPair(t)
		p.seed(t)
		p.expect(t, p.workH, minimal(t, "same", nil), 202)
		p.expect(t, p.workH, doc(t, map[string]any{"document": "same"}), 202)
	})
	t.Run("another project may reuse an id", func(t *testing.T) {
		p := newPair(t)
		p.seed(t)
		p.expect(t, p.workH, with(t, fixture(t, "project"), map[string]any{"project": "second"}), 202)
		p.expect(t, p.workH, minimal(t, "d1", nil), 202)
		p.expect(t, p.workH, minimal(t, "d1", map[string]any{"project": "second"}), 202)
	})
}

func TestConcurrentPostsOfOneIDToThePair(t *testing.T) {
	for _, kind := range []string{"decision", "document"} {
		t.Run(kind, func(t *testing.T) {
			for round := range 20 {
				p := newPair(t)
				p.seed(t)
				id := "race-" + string(rune('a'+round))
				body := func(workspace string) string {
					if kind == "decision" {
						return minimal(t, id, map[string]any{"workstream": workspace})
					}
					return doc(t, map[string]any{"document": id, "workstream": workspace})
				}
				var wg sync.WaitGroup
				codes := make([]int, 2)
				for i, h := range []http.Handler{p.workH, p.ownerH} {
					wg.Add(1)
					go func() {
						defer wg.Done()
						codes[i] = post(t, h, "factory-token", body([]string{"retry-policy", "other-stream"}[i])).Code
					}()
				}
				wg.Wait()
				oneEach := (codes[0] == 202 && codes[1] == 409) || (codes[0] == 409 && codes[1] == 202)
				if !oneEach {
					t.Fatalf("round %d: codes = %v, want one 202 and one 409", round, codes)
				}
				held := 0
				for _, source := range []string{sourceID, ownerID} {
					events, _ := p.store.CurrentArtifacts(t.Context(), source)
					for _, ev := range events {
						if h, ok := work.Decode(ev); ok && h.ID == id {
							held++
						}
					}
				}
				if held != 1 {
					t.Fatalf("round %d: %d records hold %s, want exactly one", round, held, id)
				}
			}
		})
	}
}

func TestSupersession(t *testing.T) {
	t.Run("a decision that is not held", func(t *testing.T) {
		p := newPair(t)
		p.seed(t)
		before := len(p.store.history())
		r := p.expect(t, p.workH, minimal(t, "b", map[string]any{"supersedes": "a"}), 409)
		if !strings.Contains(r.Body, "not held") || len(p.store.history()) != before {
			t.Errorf("409 %q or a write", r.Body)
		}
		// Another project's decision is not held for this one.
		p.expect(t, p.workH, with(t, fixture(t, "project"), map[string]any{"project": "second"}), 202)
		p.expect(t, p.workH, minimal(t, "a", map[string]any{"project": "second"}), 202)
		p.expect(t, p.workH, minimal(t, "b", map[string]any{"supersedes": "a"}), 409)
		// A document under that id is not a decision.
		p.expect(t, p.workH, doc(t, map[string]any{"document": "doc-a"}), 202)
		p.expect(t, p.workH, minimal(t, "b", map[string]any{"supersedes": "doc-a"}), 409)
	})

	places := []struct {
		name  string
		from  func(*pair) http.Handler
		fromW string
		to    func(*pair) http.Handler
		toW   string
	}{
		{"work source over owner source", func(p *pair) http.Handler { return p.ownerH }, "w1", func(p *pair) http.Handler { return p.workH }, "w1"},
		{"owner source over work source", func(p *pair) http.Handler { return p.workH }, "w1", func(p *pair) http.Handler { return p.ownerH }, "w1"},
		{"across workstreams", func(p *pair) http.Handler { return p.workH }, "w1", func(p *pair) http.Handler { return p.workH }, "w2"},
		{"workstream to project", func(p *pair) http.Handler { return p.workH }, "w1", func(p *pair) http.Handler { return p.workH }, ""},
		{"project to workstream", func(p *pair) http.Handler { return p.workH }, "", func(p *pair) http.Handler { return p.workH }, "w2"},
		{"across workstreams and sources", func(p *pair) http.Handler { return p.ownerH }, "w1", func(p *pair) http.Handler { return p.workH }, ""},
	}
	for _, tt := range places {
		t.Run(tt.name, func(t *testing.T) {
			p := newPair(t)
			p.seed(t)
			p.expect(t, p.workH, with(t, fixture(t, "workstream"), map[string]any{"workstream": "w1", "name": "W1"}), 202)
			p.expect(t, p.workH, with(t, fixture(t, "workstream"), map[string]any{"workstream": "w2", "name": "W2"}), 202)
			ws := func(w string) map[string]any {
				if w == "" {
					return map[string]any{"workstream": nil}
				}
				return map[string]any{"workstream": w}
			}
			p.expect(t, tt.from(p), minimal(t, "old", ws(tt.fromW)), 202)
			p.expect(t, tt.to(p), minimal(t, "new", mergeMaps(ws(tt.toW), map[string]any{"supersedes": "old"})), 202)
			// The target is the one held record, and the reference records where.
			var newest connector.Event
			for _, source := range []string{sourceID, ownerID} {
				events, _ := p.store.CurrentArtifacts(t.Context(), source)
				for _, ev := range events {
					if h, ok := work.Decode(ev); ok && h.ID == "new" {
						newest = ev
					}
				}
			}
			h, _ := work.Decode(newest)
			old := h.Native().SupersedesAt
			if old == nil || old.Artifact == "" {
				t.Fatalf("supersedes_at = %+v, want the resolved target", old)
			}
			if got, ok := p.heldIn(t, old.Source, old.Artifact); !ok {
				t.Fatalf("supersedes_at %+v is not held", old)
			} else if target, _ := work.Decode(got); target.ID != "old" {
				t.Fatalf("supersedes_at resolves to %q, want old", target.ID)
			}
		})
	}

	t.Run("cycles", func(t *testing.T) {
		p := newPair(t)
		p.seed(t)
		p.expect(t, p.workH, minimal(t, "a", nil), 202)
		p.expect(t, p.ownerH, minimal(t, "b", map[string]any{"supersedes": "a", "workstream": "retry-policy"}), 202)
		p.expect(t, p.workH, minimal(t, "c", map[string]any{"supersedes": "b", "workstream": "other-stream"}), 202)
		before := len(p.store.history())
		// Closed by revising the first decision to name the last.
		r := p.expect(t, p.workH, minimal(t, "a", map[string]any{"supersedes": "c", "updated_at": "2026-10-14T19:00:00Z"}), 409)
		if !strings.Contains(r.Body, "cycle") {
			t.Errorf("409 %q does not say it would close a cycle", r.Body)
		}
		// Closed by revising a middle one, on the other source.
		p.expect(t, p.ownerH, minimal(t, "b", map[string]any{"supersedes": "c", "workstream": "retry-policy", "updated_at": "2026-10-14T19:00:00Z"}), 409)
		// A two-cycle.
		p.expect(t, p.workH, minimal(t, "a", map[string]any{"supersedes": "b", "updated_at": "2026-10-14T19:00:00Z"}), 409)
		if len(p.store.history()) != before {
			t.Fatal("a refused cycle wrote")
		}
		// Revising within the chain is fine, and so is dropping a link.
		p.expect(t, p.workH, minimal(t, "c", map[string]any{"supersedes": "a", "workstream": "other-stream", "updated_at": "2026-10-14T19:00:00Z"}), 202)
		p.expect(t, p.workH, minimal(t, "a", map[string]any{"updated_at": "2026-10-14T20:00:00Z", "outcome": "Ship it, revised."}), 202)
		// The walk is over held decisions: once b is gone, a may name c.
		p.expect(t, p.ownerH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"b","deleted":true}`, 202)
		p.expect(t, p.workH, minimal(t, "a", map[string]any{"supersedes": "c", "updated_at": "2026-10-14T21:00:00Z"}), 409)
	})
}

func TestResolveAndClosesCycle(t *testing.T) {
	mk := func(source, workstream, id, supersedes string) connector.Event {
		n, _ := json.Marshal(work.Native{Project: "p", Workstream: workstream, Decision: id, Supersedes: supersedes})
		artifact := "p:decision:" + id
		if workstream != "" {
			artifact = "p#" + workstream + ":decision:" + id
		}
		return connector.Event{Source: source, Kind: work.KindDecision, Payload: connector.Payload{Artifact: artifact, Container: connector.Container{NativeID: "p"}, Native: n}}
	}
	records := work.Records([]connector.Event{mk("s", "", "a", ""), mk("o", "w", "b", "a"), mk("s", "w", "c", "b")}, "p")
	if len(records) != 3 {
		t.Fatalf("Records = %d, want 3", len(records))
	}
	got, ok, err := work.Resolve(records, work.KindDecisionRecord, "b")
	if err != nil || !ok || got.Source != "o" || got.Workstream != "w" {
		t.Fatalf("Resolve = %+v %v %v, want the owner-source one", got, ok, err)
	}
	if _, ok, _ := work.Resolve(records, work.KindDocumentRecord, "b"); ok {
		t.Error("Resolve found a document under a decision's id")
	}
	if _, ok, _ := work.Resolve(records, work.KindDecisionRecord, "zz"); ok {
		t.Error("Resolve found an unheld id")
	}
	dup := append(records, work.Records([]connector.Event{mk("o", "", "a", "")}, "p")...)
	if _, _, err := work.Resolve(dup, work.KindDecisionRecord, "a"); !errors.Is(err, work.ErrAmbiguous) {
		t.Errorf("Resolve of a doubly held id = %v, want ErrAmbiguous", err)
	}
	for _, tt := range []struct {
		id, supersedes string
		want           bool
	}{
		{"a", "c", true}, {"a", "b", true}, {"a", "a", true}, {"d", "c", false}, {"b", "a", false}, {"a", "zz", false},
	} {
		if got, err := work.ClosesCycle(records, tt.id, tt.supersedes); err != nil || got != tt.want {
			t.Errorf("ClosesCycle(%s, %s) = %v %v, want %v", tt.id, tt.supersedes, got, err, tt.want)
		}
	}
}

func TestOwnerSource(t *testing.T) {
	t.Run("refuses project, workstream and update", func(t *testing.T) {
		p := newPair(t)
		p.seed(t)
		before := len(p.store.history())
		for _, body := range []string{
			fixture(t, "project"), fixture(t, "workstream"), fixture(t, "update"),
			`{"kind":"project","project":"payments-rewrite","deleted":true}`,
			`{"kind":"update","project":"payments-rewrite","update":"u1","deleted":true}`,
		} {
			r := p.expect(t, p.ownerH, body, 400)
			if !strings.Contains(r.Body, "owner source") || !strings.Contains(r.Body, sourceID) {
				t.Errorf("400 %q does not point at the work source", r.Body)
			}
		}
		if len(p.store.history()) != before {
			t.Fatal("a refused request wrote")
		}
	})

	t.Run("accepts records for held projects and workstreams, in the project's container", func(t *testing.T) {
		p := newPair(t)
		p.seed(t)
		p.expect(t, p.ownerH, doc(t, map[string]any{"workstream": nil}), 202)
		p.expect(t, p.ownerH, doc(t, map[string]any{"document": "spec-2"}), 202)
		p.expect(t, p.ownerH, fixture(t, "decision"), 409) // supersedes d-1, which is not held
		p.expect(t, p.ownerH, minimal(t, "d-1", map[string]any{"workstream": "retry-policy", "decider": "owner"}), 202)
		p.expect(t, p.ownerH, fixture(t, "decision"), 202)
		ev, ok := p.heldIn(t, ownerID, "payments-rewrite#retry-policy:decision:d-2")
		if !ok || ev.Source != ownerID {
			t.Fatalf("owner-source decision = %v %+v", ok, ev)
		}
		if n := held(t, p.store, "payments-rewrite"); !ev.ACL.Equal(n.ACL) {
			t.Errorf("owner-source ACL %v, want the project's %v", ev.ACL, n.ACL)
		}
		if h, _ := work.Decode(ev); h.Native().WorkSource != sourceID {
			t.Errorf("work_source = %q, want %q", h.Native().WorkSource, sourceID)
		}
		// The work source holds none of them.
		if events, _ := p.store.CurrentArtifacts(t.Context(), sourceID); len(events) != 3 {
			t.Errorf("work source holds %d artifacts, want only its project and two workstreams", len(events))
		}
		// Deleting on the owner source, and a deletion not held.
		p.expect(t, p.ownerH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"d-2","deleted":true}`, 202)
		p.expect(t, p.ownerH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"d-2","deleted":true}`, 204)
	})

	t.Run("answers 409 when the project or workstream is not held on the work source", func(t *testing.T) {
		p := newPair(t)
		p.expect(t, p.ownerH, minimal(t, "d1", nil), 409)
		p.expect(t, p.ownerH, doc(t, nil), 409)
		p.seed(t)
		p.expect(t, p.ownerH, minimal(t, "d1", map[string]any{"workstream": "missing"}), 409)
		p.expect(t, p.ownerH, doc(t, map[string]any{"workstream": "missing"}), 409)
		// A project held on the owner source's own account does not count: it
		// can hold none, and a project deleted on the work source is not held.
		p.expect(t, p.workH, `{"kind":"project","project":"payments-rewrite","deleted":true}`, 202)
		p.expect(t, p.ownerH, minimal(t, "d1", nil), 409)
	})

	t.Run("a work source accepts records for its own projects without an owner source", func(t *testing.T) {
		c := build(t, source())
		s := &memoryStore{}
		h := handler(c, s)
		send(t, h, fixture(t, "project"), 202)
		send(t, h, fixture(t, "document"), 409)
		send(t, h, with(t, fixture(t, "document"), map[string]any{"workstream": nil}), 202)
		send(t, h, minimal(t, "d1", nil), 202)
	})
}

func TestNewChecksTheOwnerSetting(t *testing.T) {
	configs := func(owner string, others ...connector.SourceConfig) []connector.SourceConfig {
		src := ownerSource()
		src.Settings = json.RawMessage(`{"owner_of":"` + owner + `"}`)
		return append([]connector.SourceConfig{src}, others...)
	}
	github := connector.SourceConfig{ID: "github", Type: "github", Containers: []string{"acme/api"}}
	other := ownerSource()
	other.ID = "second-owner"
	tests := []struct {
		name    string
		src     connector.SourceConfig
		sources []connector.SourceConfig
		want    string
	}{
		{"names itself", configs(ownerID)[0], configs(ownerID), "itself"},
		{"names a source that is not a work source", configs("github")[0], configs("github", github), "not a work source"},
		{"names another owner source", configs("second-owner")[0], configs("second-owner", other), "itself an owner source"},
		{"names an unconfigured source", configs("ghost")[0], configs("ghost"), "not configured"},
		{"names a work source", configs(sourceID)[0], configs(sourceID, source()), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := work.New(tt.src, principals(), []string{sourceID, ownerID, "github", "second-owner"}, lookup, work.WithSources(tt.sources))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("New = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("New = %v, want %q", err, tt.want)
			}
		})
	}
	if got := work.OwnerOf(ownerSource()); got != sourceID {
		t.Errorf("OwnerOf(owner source) = %q", got)
	}
	if got := work.OwnerOf(source()); got != "" {
		t.Errorf("OwnerOf(work source) = %q", got)
	}
	if got := work.OwnerOf(connector.SourceConfig{Type: "github", Settings: json.RawMessage(`{"owner_of":"x"}`)}); got != "" {
		t.Errorf("OwnerOf(github) = %q", got)
	}
}

// records returns the live documents and decisions of both sources.
func (p *pair) records(t *testing.T) []connector.Event {
	t.Helper()
	var out []connector.Event
	for _, source := range []string{sourceID, ownerID} {
		events, err := p.store.CurrentArtifacts(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			if _, ok := work.Decode(ev); ok {
				out = append(out, ev)
			}
		}
	}
	return out
}

func (p *pair) assertACL(t *testing.T, want int) {
	t.Helper()
	root := held(t, p.store, "payments-rewrite")
	records := p.records(t)
	if len(records) != want {
		t.Fatalf("%d live records, want %d", len(records), want)
	}
	for _, ev := range records {
		if !ev.ACL.Equal(root.ACL) {
			t.Errorf("%s %s carries ACL %v, want the project's %v", ev.Source, ev.Payload.Artifact, ev.ACL, root.ACL)
		}
	}
}

func (p *pair) seedRecords(t *testing.T) {
	t.Helper()
	if p.private {
		// A project posted without repos is private to its poster.
		p.expect(t, p.workH, with(t, fixture(t, "project"), map[string]any{"repos": nil}), 202)
		p.expect(t, p.workH, fixture(t, "workstream"), 202)
		p.expect(t, p.workH, with(t, fixture(t, "workstream"), map[string]any{"workstream": "other-stream", "name": "Other"}), 202)
	} else {
		p.seed(t)
	}
	p.expect(t, p.workH, doc(t, nil), 202)
	p.expect(t, p.workH, minimal(t, "w-dec", map[string]any{"workstream": "retry-policy"}), 202)
	p.expect(t, p.workH, minimal(t, "w-proj", nil), 202)
	p.expect(t, p.ownerH, doc(t, map[string]any{"document": "o-doc", "workstream": nil}), 202)
	p.expect(t, p.ownerH, minimal(t, "o-dec", map[string]any{"decider": "owner", "supersedes": "w-dec", "author": "kyle"}), 202)
}

func TestRecordsFollowTheProjectsAccess(t *testing.T) {
	p := newPair(t)
	p.private = true
	p.seedRecords(t)
	root := held(t, p.store, "payments-rewrite")
	// Posted without repos: only the poster and the named author.
	if len(root.ACL) != 2 || root.ACL[0].Kind != connector.ACLIdentity || root.ACL[0].NativeID != "factory" || root.ACL[1].NativeID != "kyle" {
		t.Fatalf("project ACL = %v, want the poster and the named author", root.ACL)
	}
	p.assertACL(t, 5)

	project := func(updated string, repos ...work.Repo) string {
		fields := map[string]any{"updated_at": updated}
		if len(repos) > 0 {
			fields["repos"] = repos
		}
		return with(t, fixture(t, "project"), fields)
	}
	steps := []struct {
		name string
		body string
	}{
		{"repos added", project("2026-10-15T10:00:00Z", work.Repo{Source: "github", Project: "acme/api"})},
		{"repos replaced", project("2026-10-15T11:00:00Z", work.Repo{Source: "github-other", Project: "acme/private"})},
		{"repos removed", project("2026-10-15T12:00:00Z")},
		{"repos added again", project("2026-10-15T13:00:00Z", work.Repo{Source: "github", Project: "acme/api"})},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			p.expect(t, p.workH, step.body, 202)
			p.assertACL(t, 5)
		})
	}
	root = held(t, p.store, "payments-rewrite")
	if len(root.ACL) != 1 || root.ACL[0].Kind != connector.ACLContainers {
		t.Fatalf("project ACL = %v, want its repositories", root.ACL)
	}

	t.Run("records posted later carry the current ACL, and an attributed author is named", func(t *testing.T) {
		p.expect(t, p.ownerH, minimal(t, "late", nil), 202)
		p.assertACL(t, 6)
	})

	t.Run("a private project's records name the poster and the author", func(t *testing.T) {
		q := newPair(t)
		q.expect(t, q.workH, with(t, fixture(t, "project"), map[string]any{"author": "kyle", "repos": nil}), 202)
		q.expect(t, q.ownerH, minimal(t, "d1", nil), 202)
		ev, _ := q.heldIn(t, ownerID, "payments-rewrite:decision:d1")
		var got []string
		for _, e := range ev.ACL {
			got = append(got, e.NativeID)
		}
		if len(got) != 2 || got[0] != "factory" || got[1] != "kyle" {
			t.Errorf("ACL = %v, want the poster and the named author", got)
		}
	})
}

func TestOwnerRecordsReconciledAtStartup(t *testing.T) {
	for _, from := range []string{"work source", "owner source"} {
		t.Run("a pass of the "+from+" completes an interrupted propagation", func(t *testing.T) {
			p := newPair(t)
			p.seedRecords(t)
			restricted := with(t, fixture(t, "project"), map[string]any{"repos": []work.Repo{{Source: "github", Project: "acme/private"}}, "updated_at": "2026-10-15T17:00:00Z"})
			// Land the project alone, as a process that died before propagating.
			ctx := t.Context()
			p.store.before = func(_ context.Context, ev connector.Event) error {
				if ev.Payload.Artifact != "payments-rewrite" {
					return errors.New("interrupted")
				}
				return nil
			}
			if r := post(t, p.workH, "factory-token", restricted); r.Code != 500 {
				t.Fatalf("interrupted post = %d %s, want 500", r.Code, r.Body)
			}
			p.store.before = nil
			root := held(t, p.store, "payments-rewrite")
			stale := 0
			for _, ev := range p.records(t) {
				if !ev.ACL.Equal(root.ACL) {
					stale++
				}
			}
			if stale == 0 {
				t.Fatal("test setup: nothing was left stale")
			}
			c, gate := p.work, p.workGate
			if from == "owner source" {
				c, gate = p.owner, p.ownerGate
			}
			if err := c.Poll(ctx, gate); err != nil {
				t.Fatal(err)
			}
			if from == "owner source" {
				// Its own records only; the work source's are its to repair.
				for _, ev := range p.records(t) {
					if ev.Source == ownerID && !ev.ACL.Equal(root.ACL) {
						t.Errorf("owner-source %s is still stale", ev.Payload.Artifact)
					}
				}
				if err := p.work.Poll(ctx, p.workGate); err != nil {
					t.Fatal(err)
				}
			}
			p.assertACL(t, 5)
			// A pass with nothing to do writes nothing.
			n := len(p.store.history())
			for _, poll := range []func() error{
				func() error { return p.work.Poll(ctx, p.workGate) },
				func() error { return p.owner.Poll(ctx, p.ownerGate) },
			} {
				if err := poll(); err != nil {
					t.Fatal(err)
				}
			}
			if len(p.store.history()) != n {
				t.Error("an idle pass wrote")
			}
		})
	}

	t.Run("a pass of an owner source with no project held writes nothing", func(t *testing.T) {
		p := newPair(t)
		if err := p.owner.Poll(t.Context(), p.ownerGate); err != nil {
			t.Fatal(err)
		}
		if len(p.store.history()) != 0 {
			t.Error("wrote")
		}
	})
}

func TestRecordContractFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/connector-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"document", "decision"} {
		if !strings.Contains(string(raw), strings.TrimSpace(fixture(t, name))) {
			t.Errorf("contract is missing the %s request example", name)
		}
	}
}
