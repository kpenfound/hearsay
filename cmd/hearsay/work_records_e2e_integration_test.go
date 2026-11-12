//go:build integration

package main

// The owner/work decision path, end to end, in a running Hearsay.
//
// This test runs the shipped command, `hearsay all` — connectors, distiller,
// assertion worker and API, wired by the same code the binary runs — against a
// database of its own in the Postgres HEARSAY_DATABASE_URL names, with the
// configuration docs/config.md documents for an owner/work pair. It posts
// hand-written requests, in the shape docs/connector-contract.md documents, to
// the connectors' real HTTP listener at /hooks/<source>, and reads the result
// as people read it, through the API's real HTTP listener:
//
//   - on the work source: a project (no `repos`) and a workstream, a chief's
//     assumption, a decision that supersedes it, and a relay of an owner ruling
//     that names the owner as author and decider;
//   - on the owner source, authored by the owner through the publisher agent
//     (an attributor on both): a sealed-spec document, a charter, and owner
//     rulings.
//
// It then reads the project's and the workstream's `work:` entities with
// get_bundle and traverse as the owner, and logs every output, and checks that
// the owner ruling is served ratified, the assumption superseded by its
// successor, the successor and the relay inferred, the sealed spec a `spec`
// document, and that a configured principal who is neither the poster nor the
// owner is served what an unknown scope gets and nothing by handle.
//
// The only thing replaced is the model provider: the distill and assert tiers'
// `base_url` points at a local stub that answers the Anthropic Messages API
// with a one-line summary and no stances (no credential is reachable from a
// test, and none is needed). The stub also records every prompt, and the test
// fails if any decision's text was sent to it: decisions take no model call.
//
// Rerun it with a Postgres of your own, or inside the image's tree with Dagger
// (CONTRIBUTING.md, "The owner/work decision path, end to end"):
//
//	HEARSAY_DATABASE_URL=postgres://... go test -tags=integration -count=1 -v \
//	    -run TestWorkRecordsEndToEnd ./cmd/hearsay
//
// `dagger check hearsay:integration-test` runs it with every other integration
// test. Set HEARSAY_E2E_EVIDENCE to a directory to also write each read's raw
// output there as JSON files.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l3"
	"github.com/kpenfound/hearsay/internal/service/api"
)

// modelStub answers the Anthropic Messages API as the distill and assert tiers
// call it, and remembers what it was asked.
type modelStub struct {
	mu      sync.Mutex
	prompts []string
	schemas map[string]int
}

func (m *modelStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"input_schema"`
		} `json:"tools"`
	}
	if r.URL.Path != "/v1/messages" || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Tools) != 1 || len(req.Messages) == 0 {
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"the stub answers structured Messages calls only"}}`, http.StatusBadRequest)
		return
	}
	tool := req.Tools[0]
	text := req.Messages[0].Content
	m.mu.Lock()
	m.prompts = append(m.prompts, text)
	if m.schemas == nil {
		m.schemas = map[string]int{}
	}
	m.schemas[tool.Name]++
	m.mu.Unlock()

	var answer any
	if _, ok := tool.InputSchema.Properties["assertions"]; ok {
		answer = map[string]any{"assertions": []any{}}
	} else if _, ok := tool.InputSchema.Properties["summary"]; ok {
		first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
		answer = map[string]any{"summary": first, "outcome_kind": "none"}
	} else {
		http.Error(w, `{"error":{"type":"invalid_request_error","message":"the stub does not know this schema"}}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "msg_stub", "type": "message", "role": "assistant", "model": "stub", "stop_reason": "tool_use",
		"content": []any{map[string]any{"type": "tool_use", "id": "toolu_stub", "name": tool.Name, "input": answer}},
		"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
	})
}

// sawText reports whether any prompt the stub received contains s.
func (m *modelStub) sawText(s string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.ContainsFunc(m.prompts, func(p string) bool { return strings.Contains(p, s) })
}

// e2eConfig is the documented owner/work pair, as docs/config.md has it, with
// what a running Hearsay also needs: tokens for the people who read, one more
// person who may not read the project, and the model stub in place of the
// provider.
func e2eConfig(t *testing.T, stubURL string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "config.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)<!-- example: work-authority/hearsay.yaml -->\n```yaml\n(.*?)```").FindSubmatch(raw)
	if m == nil {
		t.Fatal("docs/config.md has no work-authority example")
	}
	text := string(m[1])
	const owner = "  - id: kyle\n    kind: human\n"
	if !strings.Contains(text, owner) {
		t.Fatalf("the documented pair no longer lists kyle as a bare human principal:\n%s", text)
	}
	text = strings.Replace(text, owner, "  - id: kyle\n    kind: human\n    token_env: HEARSAY_E2E_KYLE_TOKEN\n  - id: ann\n    kind: human\n    token_env: HEARSAY_E2E_ANN_TOKEN\n", 1)
	tier := func(name string) string {
		return fmt.Sprintf("    %s:\n      provider: anthropic\n      model: model-stub\n      base_url: %s\n      api_key_env: HEARSAY_E2E_MODEL_KEY\n      max_retries: -1\n", name, stubURL)
	}
	return text + "\nllm:\n  tiers:\n" + tier("distill") + tier("assert")
}

// freeAddr is an address nothing is listening on, for a service to take.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// scratchDatabase creates a database of its own on the server, so that the run
// reads the change feed of nothing but its own records, and drops it after.
func scratchDatabase(t *testing.T, url string) string {
	t.Helper()
	name := "e2e_work_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin, err := pgx.Connect(t.Context(), swapDatabaseName(url, "postgres"))
	if err != nil {
		t.Fatalf("connecting to the server: %v", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(t.Context())) }()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		c, err := pgx.Connect(ctx, swapDatabaseName(url, "postgres"))
		if err != nil {
			return
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return swapDatabaseName(url, name)
}

// e2eRun is a running Hearsay and the two listeners the test talks to.
type e2eRun struct {
	t        *testing.T
	hooks    string // the connectors' address
	apiAddr  string
	tokens   map[string]string
	stderr   *syncBuffer
	evidence string
}

// post sends one record to a source's push endpoint as the publisher.
func (r *e2eRun) post(source string, fields map[string]any) (int, string) {
	r.t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		r.t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, "http://"+r.hooks+"/hooks/"+source, bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.tokens["osmia"])
	req.Header.Set("Content-Type", "application/json")
	return r.do(req)
}

func (r *e2eRun) do(req *http.Request) (int, string) {
	r.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v\n%s", req.Method, req.URL, err, r.stderr.String())
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// must posts and requires 202.
func (r *e2eRun) must(source string, fields map[string]any) {
	r.t.Helper()
	if code, body := r.post(source, fields); code != http.StatusAccepted {
		r.t.Fatalf("POST /hooks/%s %v = %d %s\n%s", source, fields, code, body, r.stderr.String())
	}
	r.t.Logf("POST /hooks/%-14s 202 %-9s %v", source, fields["kind"], recordID(fields))
}

func recordID(fields map[string]any) string {
	for _, k := range []string{"decision", "document", "workstream", "project"} {
		if id, ok := fields[k]; ok {
			s := fmt.Sprint(id)
			if ws, ok := fields["workstream"]; ok && k != "workstream" {
				s += " (workstream " + fmt.Sprint(ws) + ")"
			}
			return s
		}
	}
	return ""
}

// read calls an API tool as a person.
func (r *e2eRun) read(who, call string, args any) (int, []byte) {
	r.t.Helper()
	body, _ := json.Marshal(args)
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, "http://"+r.apiAddr+"/v1/"+call, bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.PrincipalHeader, who)
	req.Header.Set(api.AuthorizationHeader, "Bearer "+r.tokens[who])
	code, got := r.do(req)
	return code, []byte(got)
}

// keep logs a read's output, and writes it to HEARSAY_E2E_EVIDENCE when set.
func (r *e2eRun) keep(name string, raw []byte) {
	r.t.Helper()
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(raw)
	}
	r.t.Logf("---- %s ----\n%s", name, pretty.String())
	if r.evidence != "" {
		_ = os.WriteFile(filepath.Join(r.evidence, name+".json"), append(pretty.Bytes(), '\n'), 0o644)
	}
}

func (r *e2eRun) bundle(who, scope string) (bundle.Bundle, []byte) {
	r.t.Helper()
	code, raw := r.read(who, "get_bundle", map[string]string{"scope": scope})
	if code != http.StatusOK {
		r.t.Fatalf("get_bundle(%s) as %s = %d %s", scope, who, code, raw)
	}
	var b bundle.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		r.t.Fatal(err)
	}
	return b, raw
}

func (r *e2eRun) traverse(who, scope string, steps ...string) (l3.Subgraph, []byte, int) {
	r.t.Helper()
	var s []map[string]string
	for _, e := range steps {
		s = append(s, map[string]string{"edge": e})
	}
	code, raw := r.read(who, "traverse", map[string]any{"start": []map[string]string{{"type": "entity", "id": scope}}, "steps": s})
	var g l3.Subgraph
	if code == http.StatusOK {
		if err := json.Unmarshal(raw, &g); err != nil {
			r.t.Fatal(err)
		}
	}
	return g, raw, code
}

// entry is what a read serves of one decision.
type entry struct {
	Status, SupersededBy, Tier, Decider, Outcome string
}

// log is the decisions get_bundle serves for an entity, by decision id. Any
// decision served twice fails the run.
func (r *e2eRun) log(scope string, b bundle.Bundle) map[string]entry {
	r.t.Helper()
	out := map[string]entry{}
	for _, s := range b.Stances {
		if s.Decision == nil {
			continue
		}
		if _, dup := out[s.Decision.ID]; dup {
			r.t.Errorf("decision %s is served twice by get_bundle(%s)", s.Decision.ID, scope)
		}
		out[s.Decision.ID] = entry{s.Decision.Status, s.Decision.SupersededBy, s.Tier, s.Decision.Decider, s.Current}
	}
	return out
}

// traversedLog is the same log as traverse serves it: a decision is a topic
// of its own, with its tier, and one stance.
func (r *e2eRun) traversedLog(scope string, g l3.Subgraph) map[string]entry {
	r.t.Helper()
	out := map[string]entry{}
	for _, n := range g.Nodes {
		if n.Type != "topic" || n.Decision == nil {
			continue
		}
		if _, dup := out[n.Decision.ID]; dup {
			r.t.Errorf("decision %s is served twice by traverse(%s)", n.Decision.ID, scope)
		}
		out[n.Decision.ID] = entry{n.Decision.Status, n.Decision.SupersededBy, n.Tier, n.Decision.Decider, fmt.Sprint(n.Current)}
	}
	return out
}

func TestWorkRecordsEndToEnd(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}

	stub := &modelStub{}
	stubServer := httptest.NewServer(stub)
	t.Cleanup(stubServer.Close)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hearsay.yaml"), []byte(e2eConfig(t, stubServer.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{"osmia": "e2e-osmia-token", "kyle": "e2e-kyle-token", "ann": "e2e-ann-token"}
	t.Setenv("HEARSAY_DATABASE_URL", scratchDatabase(t, url))
	t.Setenv("HEARSAY_OSMIA_TOKEN", tokens["osmia"])
	t.Setenv("HEARSAY_E2E_KYLE_TOKEN", tokens["kyle"])
	t.Setenv("HEARSAY_E2E_ANN_TOKEN", tokens["ann"])
	t.Setenv("HEARSAY_E2E_MODEL_KEY", "model-stub-key")
	t.Setenv("HEARSAY_DISTILL_QUIET", "0") // no debounce: the run waits for nothing but the work

	r := &e2eRun{t: t, hooks: freeAddr(t), apiAddr: freeAddr(t), tokens: tokens, stderr: &syncBuffer{}, evidence: os.Getenv("HEARSAY_E2E_EVIDENCE")}
	if r.evidence != "" {
		if err := os.MkdirAll(r.evidence, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	distillerAddr, assertAddr := freeAddr(t), freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"all", "--config", dir, "--listen", r.hooks, "--api-listen", r.apiAddr,
			"--distiller-listen", distillerAddr, "--assert-worker-listen", assertAddr, "--log-format", "json"}, io.Discard, r.stderr)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("hearsay all stopped with %v\n%s", err, r.stderr.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("hearsay all did not stop")
		}
	}
	t.Cleanup(stop)

	container := "not detected as a container"
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			container = "container (" + marker + " exists)"
		}
	}
	t.Logf("EVIDENCE: `hearsay all` (connectors %s, API %s, distiller, assertion worker), run in-process by cmd/hearsay's run() against Postgres, scratch database; environment: %s", r.hooks, r.apiAddr, container)
	t.Logf("EVIDENCE: the model provider is replaced by a local stub at %s for the distill and assert tiers (base_url); it answers a one-line summary and no stances", stubServer.URL)

	for _, addr := range []string{r.hooks, r.apiAddr, distillerAddr, assertAddr} {
		deadline := time.Now().Add(60 * time.Second)
		for {
			resp, err := http.Get("http://" + addr + "/healthz")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never answered /healthz:\n%s", addr, r.stderr.String())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// The records, as hand-written requests in the documented shape. The
	// publisher (osmia) posts them all; the owner is the author of the project
	// and of what the owner source holds, through the attributor mechanism.
	const (
		project = "payments"
		stream  = "retry"
		proj    = "work:projects:" + project
		strm    = proj + "#" + stream
	)
	ts := func(n int) string {
		return time.Date(2026, 11, 1, 12, n, 0, 0, time.UTC).Format(time.RFC3339)
	}
	r.must("projects", map[string]any{"kind": "project", "project": project, "name": "Payments rewrite", "description": "Move settlement off the legacy queue.", "author": "kyle", "created_at": ts(0), "updated_at": ts(0)})
	r.must("projects", map[string]any{"kind": "workstream", "project": project, "workstream": stream, "name": "Retry policy", "created_at": ts(1)})
	r.must("projects-owner", map[string]any{"kind": "document", "project": project, "document": "charter", "name": "Charter", "body": "# Charter\n\nPayments never settle twice.", "author": "kyle", "created_at": ts(2), "updated_at": ts(2)})
	r.must("projects-owner", map[string]any{"kind": "document", "project": project, "workstream": stream, "document": "seal-1", "name": "Retry policy sealed spec", "body": "# Retry policy\n\nCap retries at 5 with jittered backoff.\n\n## Out of scope\n\nThe queue's internals.", "author": "kyle", "created_at": ts(3), "updated_at": ts(3)})
	r.must("projects-owner", map[string]any{"kind": "decision", "project": project, "decision": "ruling-0", "outcome": "Settle through the new queue only.", "decider": "owner", "author": "kyle", "created_at": ts(4), "updated_at": ts(4)})
	r.must("projects-owner", map[string]any{"kind": "decision", "project": project, "workstream": stream, "decision": "ruling-1", "context": "Settlement calls fail transiently under load.", "outcome": "Cap retries at five attempts.", "rationale": "Five attempts cover the observed outage window.", "rejected_options": []string{"Unbounded retries"}, "decider": "owner", "spec_document": "seal-1", "question": "q-1", "author": "kyle", "created_at": ts(5), "updated_at": ts(5)})
	r.must("projects", map[string]any{"kind": "decision", "project": project, "workstream": stream, "decision": "assume-1", "outcome": "Assume a retry cap of three until the owner rules.", "decider": "chief_of_staff", "created_at": ts(6), "updated_at": ts(6)})
	r.must("projects", map[string]any{"kind": "decision", "project": project, "workstream": stream, "decision": "assume-2", "outcome": "Assume a retry cap of four, pending the sealed spec.", "decider": "chief_of_staff", "supersedes": "assume-1", "created_at": ts(7), "updated_at": ts(7)})
	r.must("projects", map[string]any{"kind": "decision", "project": project, "workstream": stream, "decision": "relay-1", "outcome": "Relayed: the owner wants jitter on every retry.", "decider": "owner", "author": "kyle", "created_at": ts(8), "updated_at": ts(8)})

	// A refusal is as observable as an acceptance: the id of a decision held on
	// the work source cannot be held again on the owner source.
	code, body := r.post("projects-owner", map[string]any{"kind": "decision", "project": project, "workstream": stream, "decision": "assume-1", "outcome": "Again.", "decider": "owner", "author": "kyle", "created_at": ts(9), "updated_at": ts(9)})
	if code != http.StatusConflict || !strings.Contains(body, "assume-1") {
		t.Errorf("a decision id held on the work source, posted on the owner source = %d %s, want 409 saying where it is held", code, body)
	}
	t.Logf("POST /hooks/projects-owner decision assume-1 (already held on projects) -> %d %s", code, strings.TrimSpace(body))

	want := map[string]entry{
		"ruling-1": {"current", "", "ratified", "owner", "Cap retries at five attempts."},
		"assume-1": {"superseded", "assume-2", "inferred", "chief_of_staff", "Assume a retry cap of three until the owner rules."},
		"assume-2": {"current", "", "inferred", "chief_of_staff", "Assume a retry cap of four, pending the sealed spec."},
		"relay-1":  {"current", "", "inferred", "owner", "Relayed: the owner wants jitter on every retry."},
	}
	// The workstream's read also serves the project's decisions, which it inherits.
	wantStream := map[string]entry{"ruling-0": {"current", "", "ratified", "owner", "Settle through the new queue only."}}
	for k, v := range want {
		wantStream[k] = v
	}
	wantProject := map[string]entry{"ruling-0": wantStream["ruling-0"]}

	// Wait for the distiller and the assertion worker to have placed all of it.
	var last map[string]entry
	deadline := time.Now().Add(3 * time.Minute)
	for {
		b, _ := r.bundle("kyle", strm)
		last = r.log(strm, b)
		pb, _ := r.bundle("kyle", proj)
		if fmt.Sprint(last) == fmt.Sprint(wantStream) && fmt.Sprint(r.log(proj, pb)) == fmt.Sprint(wantProject) && len(b.Anchors) > 0 && len(pb.Anchors) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the records were not served as expected within 3 minutes.\nserved on %s: %v\nwant: %v\n%s", strm, last, wantStream, r.stderr.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// ---- the owner's reads, recorded ----
	var specIDs []string
	for _, c := range []struct {
		scope string
		name  string
		want  map[string]entry
		spec  string // the heading of a document the entity serves as an anchor
		body  string // what that section says
	}{{proj, "project", wantProject, "# Charter", "Payments never settle twice."}, {strm, "workstream", wantStream, "# Retry policy", "Cap retries at 5 with jittered backoff."}} {
		b, raw := r.bundle("kyle", c.scope)
		r.keep("get_bundle-"+c.name+"-as-kyle", raw)
		if got := r.log(c.scope, b); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("get_bundle(%s) serves %v, want %v", c.scope, got, c.want)
		}
		if len(b.Conflicts) > 0 {
			t.Errorf("get_bundle(%s) serves conflicts %+v: a supersession is a replacement, not a disagreement", c.scope, b.Conflicts)
		}
		for _, s := range b.Stances {
			if s.Tier == "contested" {
				t.Errorf("get_bundle(%s) serves %q as contested", c.scope, s.Current)
			}
		}
		// The sealed spec (and the charter) are `spec` documents, anchors of the entity.
		found := false
		for _, a := range b.Anchors {
			if a.Line != c.spec {
				continue
			}
			found = true
			specIDs = append(specIDs, a.L1)
			code, raw := r.read("kyle", "get_l1", map[string]string{"id": a.L1})
			var doc l1.Stored
			if code != http.StatusOK || json.Unmarshal(raw, &doc) != nil {
				t.Errorf("get_l1(%s) = %d %s", a.L1, code, raw)
				continue
			}
			if !strings.Contains(doc.RawText, c.body) {
				t.Errorf("anchor %s of %s says %q, want %q in it", a.L1, c.scope, doc.RawText, c.body)
			}
			if doc.ArtifactClass != config.ArtifactSpec || doc.Source.System != "projects-owner" || !slices.Contains(doc.Scope, c.scope) {
				t.Errorf("anchor %s of %s = class %s source %s scope %v, want a spec from projects-owner about the entity", a.L1, c.scope, doc.ArtifactClass, doc.Source.System, doc.Scope)
			}
		}
		if !found {
			t.Errorf("get_bundle(%s) has no anchor %q: %+v", c.scope, c.spec, b.Anchors)
		}

		g, raw, code := r.traverse("kyle", c.scope, "topics", "stances")
		if code != http.StatusOK {
			t.Fatalf("traverse(%s, topics, stances) as kyle = %d %s", c.scope, code, raw)
		}
		r.keep("traverse-"+c.name+"-topics-stances-as-kyle", raw)
		if got := r.traversedLog(c.scope, g); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("traverse(%s) serves %v, want %v", c.scope, got, c.want)
		}
		g, raw, code = r.traverse("kyle", c.scope, "documents")
		if code != http.StatusOK {
			t.Fatalf("traverse(%s, documents) as kyle = %d %s", c.scope, code, raw)
		}
		r.keep("traverse-"+c.name+"-documents-as-kyle", raw)
		specs := 0
		for _, n := range g.Nodes {
			if n.Type == "document" && n.Class == string(config.ArtifactSpec) {
				specs++
			}
		}
		if specs == 0 {
			t.Errorf("traverse(%s, documents) serves no spec document: %s", c.scope, raw)
		}
	}

	// ---- a principal who is neither the poster nor the owner reads nothing ----
	_, unknown := r.read("ann", "get_bundle", map[string]string{"scope": "work:projects:no-such-project"})
	for _, scope := range []string{proj, strm} {
		code, raw := r.read("ann", "get_bundle", map[string]string{"scope": scope})
		r.keep("get_bundle-"+map[bool]string{false: "project", true: "workstream"}[strings.Contains(scope, "#")]+"-as-ann", raw)
		if code != http.StatusOK || strings.ReplaceAll(string(raw), scope, "SCOPE") != strings.ReplaceAll(string(unknown), "work:projects:no-such-project", "SCOPE") {
			t.Errorf("ann reads get_bundle(%s) = %d %s, want what an unknown scope gets: %s", scope, code, raw, unknown)
		}
		for _, steps := range [][]string{{"topics", "stances"}, {"documents"}} {
			if _, raw, code := r.traverse("ann", scope, steps...); code == http.StatusOK {
				t.Errorf("ann traverses %s %v: %s", scope, steps, raw)
			} else {
				t.Logf("traverse(%s, %v) as ann = %d %s", scope, steps, code, strings.TrimSpace(string(raw)))
			}
		}
		code, raw = r.read("ann", "search", map[string]any{"query": "retries", "scope": scope})
		if strings.Contains(string(raw), "retries") && !strings.Contains(string(raw), `"documents":[]`) {
			t.Errorf("ann searches %s = %d %s, want nothing", scope, code, raw)
		}
	}
	// A direct fetch by handle, of every document and decision kyle was served.
	ids := slices.Clone(specIDs)
	b, _ := r.bundle("kyle", strm)
	for _, s := range b.Stances {
		if s.Decision != nil {
			ids = append(ids, s.Decision.L1)
		}
	}
	for _, id := range ids {
		if code, raw := r.read("kyle", "get_l1", map[string]string{"id": id}); code != http.StatusOK {
			t.Errorf("kyle fetches %s = %d %s, want it served", id, code, raw)
		}
		if code, raw := r.read("ann", "get_l1", map[string]string{"id": id}); code == http.StatusOK {
			t.Errorf("ann fetches %s = %d %s, want it refused", id, code, raw)
		}
	}
	t.Logf("direct fetch (get_l1) of %d spec/decision documents: served to kyle, refused to ann", len(ids))

	// ---- decisions take no model call ----
	for _, text := range []string{"Settle through the new queue only.", "Cap retries at five attempts.", "Assume a retry cap of three", "Assume a retry cap of four", "Relayed: the owner wants jitter"} {
		if stub.sawText(text) {
			t.Errorf("the model stub was sent decision text %q: a decision must be placed with no model call", text)
		}
	}
	stub.mu.Lock()
	t.Logf("EVIDENCE: model stub received %v calls (by schema); none carried a decision's text", stub.schemas)
	stub.mu.Unlock()

	stop()
	if t.Failed() {
		t.Logf("hearsay all log:\n%s", r.stderr.String())
	}
}
