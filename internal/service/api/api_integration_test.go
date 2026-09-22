//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The database outlives a test, so every test takes a source id nothing else
// uses and puts everything it reads under it.
var sources atomic.Int64

func newSource() string {
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(sources.Add(1), 36)
}

var day = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

// world is one scope's worth of data: a tracker item with its issue, a pull
// request on it, a private issue only kyle may read, the events they cite and a
// topic with two stances.
type world struct {
	src, scope, project string
	issue, pr, secret   string
	server              *httptest.Server
	calls               *api.Calls
	pool                *pgxpool.Pool
	// putIn writes an event and its document, about an entity of the caller's
	// choosing and the project's code entity, as l1.Build would.
	putIn func(scope, artifact string, kind l1.Kind, hour int, summary string, acl connector.ACL, questions ...string) string
}

const kyleNode = "MDQ6VXNlcjE="

func repo(src string) config.Repo {
	return config.Repo{
		Principals: []principal.Principal{
			{ID: "kyle", Kind: principal.KindHuman, TokenEnv: "HEARSAY_TEST_KYLE_TOKEN", Identities: []principal.Identity{{Source: src, NativeID: kyleNode, Handle: "kpenfound"}}},
			{ID: "sam", Kind: principal.KindHuman, TokenEnv: "HEARSAY_TEST_SAM_TOKEN", Identities: []principal.Identity{{Source: src, NativeID: "MDQ6VXNlcjI=", Handle: "sam"}}},
			{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "HEARSAY_TEST_SHED_TOKEN", Identities: []principal.Identity{{Source: src, Handle: "shed[bot]"}}},
		},
	}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	t.Setenv("HEARSAY_TEST_KYLE_TOKEN", "test-kyle-api-credential")
	t.Setenv("HEARSAY_TEST_SAM_TOKEN", "test-sam-api-credential")
	t.Setenv("HEARSAY_TEST_SHED_TOKEN", "test-shed-api-credential")
	pool := newPool(t)
	w := &world{src: newSource(), pool: pool}
	w.project = "acme/" + w.src
	w.scope = "tracker:" + w.src + ":" + w.project + "#12"
	ctx := t.Context()
	events := l0.New(pool)
	docs := l1.New(pool)
	graph := l2.New(pool)

	public := connector.ACL{{Kind: connector.ACLPublic}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode, Label: "kpenfound"}}

	w.putIn = func(scope, artifact string, kind l1.Kind, hour int, summary string, acl connector.ACL, questions ...string) string {
		t.Helper()
		ev := connector.Event{
			Source: w.src, NativeID: artifact, Kind: connector.KindIssue, Time: day.Add(time.Duration(hour) * time.Hour),
			Payload: connector.Payload{
				Artifact: artifact, Title: summary,
				Container: connector.Container{Kind: connector.ContainerRepository, NativeID: w.project},
				Author:    &connector.Identity{Source: w.src, Kind: connector.IdentityUser, NativeID: kyleNode},
			},
			ACL: acl,
		}
		if kind == l1.KindPR {
			ev.Kind = connector.KindPullRequest
		}
		if _, err := events.Append(ctx, ev); err != nil {
			t.Fatalf("Append(%s) = %v", artifact, err)
		}
		at := day.Add(time.Duration(hour) * time.Hour)
		doc := l1.Document{
			ID: l1.DocID(w.src, artifact), Kind: kind,
			Source: l1.Source{System: w.src, NativeID: artifact},
			L0Refs: []string{connector.EventID(w.src, artifact)},
			Time:   l1.Times{Created: at, Updated: at, LastActivity: at},
			Scope:  []string{scope, "code:" + w.project},
			ACL:    acl, Text: summary, RawText: summary,
			Body: l1.Body{Summary: summary, OutcomeKind: l1.OutcomeDecided, OpenQuestions: questions},
		}
		if _, err := docs.Put(ctx, doc); err != nil {
			t.Fatalf("Put(%s) = %v", doc.ID, err)
		}
		return doc.ID
	}
	put := func(artifact string, kind l1.Kind, hour int, summary string, acl connector.ACL, questions ...string) string {
		t.Helper()
		return w.putIn(w.scope, artifact, kind, hour, summary, acl, questions...)
	}
	w.issue = put(w.project+"#12", l1.KindIssue, 1, "Move the lock out of the request path.", public, "who runs the migration in staging?")
	w.pr = put(w.project+"#13", l1.KindPR, 2, "Moves the lock into the worker; merged.", public)
	w.secret = put(w.project+"#14", l1.KindIssue, 3, "The customer behind the outage.", private, "do we tell them?")

	if err := graph.PutEntity(ctx, l2.Entity{ID: "code:" + w.project, Type: l2.TypeProject, Name: w.project, Owners: []string{"kyle"}, Origin: l2.OriginConfig, PartOf: []string{"code:" + w.src + "-org"}}); err != nil {
		t.Fatal(err)
	}
	if err := graph.PutEntity(ctx, l2.Entity{ID: "code:" + w.src + "-org", Type: l2.TypeProject, Name: "org", Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	topic := l2.Topic{ID: l2.TopicID(w.src, w.issue, 0, "where the lock lives"), Scope: w.src, Name: "where the lock lives",
		About: []string{w.scope}, ACL: public, OpenedBy: w.issue}
	inherited := l2.Topic{ID: l2.TopicID(w.src, w.pr, 0, "how the org deploys"), Scope: w.src, Name: "how the org deploys",
		About: []string{"code:" + w.src + "-org"}, ACL: public, OpenedBy: w.pr}
	hidden := l2.Topic{ID: l2.TopicID(w.src, w.secret, 0, "the customer"), Scope: w.src, Name: "the customer",
		About: []string{w.scope}, ACL: private, OpenedBy: w.secret}
	for _, tp := range []l2.Topic{topic, inherited, hidden} {
		if _, err := graph.OpenTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
	}
	stance := func(tp l2.Topic, docID, position string, hour int, tier l2.Tier, acl connector.ACL) {
		t.Helper()
		_, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(tp.ID, docID, position), TopicID: tp.ID, Position: position, Author: "kyle",
			StatedAt: day.Add(time.Duration(hour) * time.Hour), Evidence: []string{docID}, Tier: tier, ACL: acl,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A private stance on a public topic, older than everything else on it.
	stance(topic, w.secret, "an early private note", 0, l2.TierInferred, private)
	stance(topic, w.issue, "in the request path, behind a flag", 1, l2.TierInferred, public)
	stance(topic, w.pr, "in the worker", 2, l2.TierRatified, public)
	stance(inherited, w.pr, "deploys go through the queue", 2, l2.TierInferred, public)
	stance(hidden, w.secret, "we name them", 3, l2.TierInferred, private)

	calls, err := api.NewCalls(pool, repo(w.src), nil)
	if err != nil {
		t.Fatal(err)
	}
	w.calls = calls
	w.server = httptest.NewServer(api.Handler(calls, pool))
	t.Cleanup(w.server.Close)
	return w
}

func (w *world) post(t *testing.T, path string, caller api.Caller, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if caller.Principal != "" {
		req.Header.Set(api.PrincipalHeader, caller.Principal)
		switch caller.Principal {
		case "kyle":
			req.Header.Set(api.AuthorizationHeader, "Bearer test-kyle-api-credential")
		case "sam":
			req.Header.Set(api.AuthorizationHeader, "Bearer test-sam-api-credential")
		case "shed":
			req.Header.Set(api.AuthorizationHeader, "Bearer test-shed-api-credential")
		}
	}
	if caller.Agent != "" {
		req.Header.Set(api.AgentHeader, caller.Agent)
		switch caller.Agent {
		case "sam":
			req.Header.Set(api.AgentTokenHeader, "test-sam-api-credential")
		case "shed":
			req.Header.Set(api.AgentTokenHeader, "test-shed-api-credential")
		}
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

// http calls over HTTP and fails the test on anything but a 200.
func (w *world) http(t *testing.T, caller api.Caller, call string, args any) []byte {
	t.Helper()
	body, _ := json.Marshal(args)
	status, got := w.post(t, "/v1/"+call, caller, string(body))
	if status != http.StatusOK {
		t.Fatalf("POST /v1/%s = %d %s", call, status, got)
	}
	return got
}

// mcp calls the same over MCP and returns the tool result's text.
func (w *world) mcp(t *testing.T, caller api.Caller, call string, args any) ([]byte, bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": call, "arguments": args},
	})
	status, got := w.post(t, "/mcp", caller, string(body))
	if status != http.StatusOK {
		t.Fatalf("POST /mcp = %d %s", status, got)
	}
	var resp struct {
		Result api.ToolResult `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(got, &resp); err != nil || resp.Error != nil || len(resp.Result.Content) != 1 {
		t.Fatalf("tools/call answered %s", got)
	}
	return []byte(resp.Result.Content[0].Text), resp.Result.IsError
}

var (
	kyle     = api.Caller{Principal: "kyle"}
	sam      = api.Caller{Principal: "sam"}
	shedKyle = api.Caller{Principal: "kyle", Agent: "shed"}
)

func decodeBundle(t *testing.T, body []byte) bundle.Bundle {
	t.Helper()
	var b bundle.Bundle
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("not a bundle: %v: %s", err, body)
	}
	return b
}

// The acceptance criterion: the same request over MCP and HTTP returns
// byte-identical bundles — and the same for every other call, and for a
// refusal.
func TestTheSameRequestIsByteIdenticalOverMCPAndHTTP(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name   string
		caller api.Caller
		call   string
		args   map[string]any
	}{
		{"get_bundle", kyle, "get_bundle", map[string]any{"scope": w.scope}},
		{"get_bundle for an agent", shedKyle, "get_bundle", map[string]any{"scope": w.scope}},
		{"get_bundle for another reader", sam, "get_bundle", map[string]any{"scope": w.scope}},
		{"get_l1", kyle, "get_l1", map[string]any{"id": w.issue}},
		{"get_l0", kyle, "get_l0", map[string]any{"id": connector.EventID(w.src, w.project+"#12")}},
		{"stance_history", kyle, "stance_history", map[string]any{"topic": l2.TopicID(w.src, w.issue, 0, "where the lock lives")}},
		{"search", kyle, "search", map[string]any{"query": "lock", "scope": w.scope}},
		{"resolve", kyle, "resolve", map[string]any{"text": "the " + w.project + " project"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overHTTP := w.http(t, tc.caller, tc.call, tc.args)
			overMCP, isError := w.mcp(t, tc.caller, tc.call, tc.args)
			if isError {
				t.Fatalf("MCP says the call failed: %s", overMCP)
			}
			if !bytes.Equal(overHTTP, overMCP) {
				t.Errorf("HTTP and MCP differ:\nHTTP %s\nMCP  %s", overHTTP, overMCP)
			}
		})
	}

	t.Run("a refusal", func(t *testing.T) {
		status, overHTTP := w.post(t, "/v1/get_l1", kyle, `{"id":"l1:nobody:nothing"}`)
		overMCP, isError := w.mcp(t, kyle, "get_l1", map[string]any{"id": "l1:nobody:nothing"})
		if status != http.StatusNotFound || !isError || !bytes.Equal(overHTTP, overMCP) {
			t.Errorf("HTTP %d %s, MCP isError=%v %s: want a 404 and the same body", status, overHTTP, isError, overMCP)
		}
	})
}

// The acceptance criterion: same scope, same principal, no new events yields
// the same bundle; a new event on the scope changes it.
func TestABundleChangesOnlyWhenTheScopeDoes(t *testing.T) {
	w := newWorld(t)
	args := map[string]any{"scope": w.scope}
	first := w.http(t, kyle, "get_bundle", args)
	again := w.http(t, kyle, "get_bundle", args)
	if !bytes.Equal(first, again) {
		t.Fatalf("two bundles with nothing new between them differ:\n%s\n%s", first, again)
	}

	// Serving a bundle writes an audit event; that is an event, and it is not
	// one on the scope, so it must not move the bundle.
	docs := l1.New(w.pool)
	at := day.Add(5 * time.Hour)
	artifact := w.project + "#15"
	if _, err := docs.Put(t.Context(), l1.Document{
		ID: l1.DocID(w.src, artifact), Kind: l1.KindPR, Source: l1.Source{System: w.src, NativeID: artifact},
		L0Refs: []string{connector.EventID(w.src, artifact)}, Time: l1.Times{Created: at, Updated: at, LastActivity: at},
		Scope: []string{w.scope}, ACL: connector.ACL{{Kind: connector.ACLPublic}},
		Text: "a follow-up", RawText: "a follow-up", Body: l1.Body{Summary: "A follow-up to the lock move.", OutcomeKind: l1.OutcomeNone},
	}); err != nil {
		t.Fatal(err)
	}
	changed := w.http(t, kyle, "get_bundle", args)
	if bytes.Equal(first, changed) {
		t.Fatalf("a new document on the scope did not change the bundle: %s", changed)
	}
	if b := decodeBundle(t, changed); b.Recent.Items[0].L1 != l1.DocID(w.src, artifact) || b.Recent.LastActivity != "2026-09-05T15:00:00Z" {
		t.Errorf("recent = %+v, want the new document first", b.Recent)
	}
}

// The acceptance criterion: every line in a bundle carries an L1 or L0 id that
// resolves through get_l1 or get_l0 — for the reader it was served to.
func TestEveryLineCarriesAnIDThatResolves(t *testing.T) {
	w := newWorld(t)
	for _, caller := range []api.Caller{kyle, sam, shedKyle} {
		t.Run(caller.Principal+"/"+caller.Agent, func(t *testing.T) {
			body := w.http(t, caller, "get_bundle", map[string]any{"scope": w.scope})
			var lines, followed int
			follow := func(where string, ids ...string) {
				t.Helper()
				lines++
				if len(ids) == 0 {
					t.Errorf("%s carries a line and no id", where)
				}
				for _, id := range ids {
					followed++
					var doc l1.Document
					if err := json.Unmarshal(w.http(t, caller, "get_l1", map[string]any{"id": id}), &doc); err != nil || doc.ID != id {
						t.Errorf("%s: get_l1(%s) = %+v, %v", where, id, doc, err)
					}
					for _, ref := range doc.L0Refs {
						w.http(t, caller, "get_l0", map[string]any{"id": ref})
					}
				}
			}
			b := decodeBundle(t, body)
			for _, e := range b.Scope.Entities {
				if e.Line != "" {
					follow("entity "+e.ID, e.L1)
				}
			}
			for _, s := range b.Stances {
				follow("stance on "+s.Topic, s.Evidence...)
			}
			for _, item := range b.Recent.Items {
				follow("recent "+item.L1, item.L1)
			}
			for _, q := range b.OpenQuestions {
				follow("open question "+q.Line, q.Evidence...)
			}
			if lines < 5 {
				t.Errorf("the bundle has %d lines; the test is not looking at one: %s", lines, body)
			}
		})
	}
}

// Every read is filtered by principal: what kyle may read and sam may not is
// nowhere in sam's bundle, and is refused by the handles the way a missing id is.
func TestEveryReadIsFilteredByPrincipal(t *testing.T) {
	w := newWorld(t)
	kyles := string(w.http(t, kyle, "get_bundle", map[string]any{"scope": w.scope}))
	sams := string(w.http(t, sam, "get_bundle", map[string]any{"scope": w.scope}))
	for _, secret := range []string{w.secret, "the customer", "we name them", "do we tell them?"} {
		if !strings.Contains(kyles, secret) {
			t.Errorf("kyle's bundle has no %q, which kyle may read: %s", secret, kyles)
		}
		if strings.Contains(sams, secret) {
			t.Errorf("sam's bundle has %q, which sam may not read: %s", secret, sams)
		}
	}
	b := decodeBundle(t, []byte(sams))
	var inherited, current bool
	for _, s := range b.Stances {
		inherited = inherited || s.Topic == "how the org deploys" && s.Inherited
		current = current || s.Topic == "where the lock lives" && s.Current == "in the worker" &&
			s.Tier == "ratified" && s.Supersedes == "in the request path, behind a flag" && !s.Inherited
	}
	if !inherited || !current {
		t.Errorf("stances = %+v, want the current ratified stance with what it superseded, and the org's inherited one", b.Stances)
	}

	refusals := []struct {
		call string
		args map[string]any
	}{
		{"get_l1", map[string]any{"id": w.secret}},
		{"get_l0", map[string]any{"id": connector.EventID(w.src, w.project+"#14")}},
		{"stance_history", map[string]any{"topic": l2.TopicID(w.src, w.secret, 0, "the customer")}},
	}
	for _, r := range refusals {
		body, _ := json.Marshal(r.args)
		if status, got := w.post(t, "/v1/"+r.call, sam, string(body)); status != http.StatusNotFound {
			t.Errorf("%s for sam = %d %s, want 404", r.call, status, got)
		}
		if status, got := w.post(t, "/v1/"+r.call, kyle, string(body)); status != http.StatusOK {
			t.Errorf("%s for kyle = %d %s, want 200", r.call, status, got)
		}
	}
	history := map[string]any{"topic": l2.TopicID(w.src, w.issue, 0, "where the lock lives")}
	samsHistory := string(w.http(t, sam, "stance_history", history))
	if strings.Contains(samsHistory, "an early private note") {
		t.Errorf("sam's history names the private stance: %s", samsHistory)
	}
	var samsStances struct {
		Stances []api.StanceRecord `json:"stances"`
	}
	_ = json.Unmarshal([]byte(samsHistory), &samsStances)
	if len(samsStances.Stances) != 2 || samsStances.Stances[0].Supersedes != "" || samsStances.Stances[1].Supersedes != samsStances.Stances[0].ID {
		t.Errorf("sam's history = %+v, want two stances, the first naming nothing it superseded", samsStances.Stances)
	}
	if kyles := string(w.http(t, kyle, "stance_history", history)); !strings.Contains(kyles, "an early private note") {
		t.Errorf("kyle's history is missing the stance kyle may read: %s", kyles)
	}

	var found struct {
		Documents []l1.Document `json:"documents"`
	}
	_ = json.Unmarshal(w.http(t, sam, "search", map[string]any{"query": "customer outage"}), &found)
	for _, d := range found.Documents {
		if d.ID == w.secret {
			t.Errorf("sam's search found %s", w.secret)
		}
	}

	for _, tc := range []struct {
		name   string
		caller api.Caller
		status int
	}{
		{"nobody", api.Caller{}, http.StatusUnauthorized},
		{"a stranger", api.Caller{Principal: "mallory"}, http.StatusForbidden},
		{"an agent as the person", api.Caller{Principal: "shed"}, http.StatusForbidden},
		{"a person as the agent", api.Caller{Principal: "kyle", Agent: "sam"}, http.StatusForbidden},
	} {
		if status, got := w.post(t, "/v1/get_bundle", tc.caller, `{"scope":"`+w.scope+`"}`); status != tc.status {
			t.Errorf("%s: get_bundle = %d %s, want %d", tc.name, status, got, tc.status)
		}
	}
	// An agent nobody configured is named as such, not as a malformed id.
	if status, got := w.post(t, "/v1/get_bundle", api.Caller{Principal: "kyle", Agent: "mallory"}, `{"scope":"`+w.scope+`"}`); status != http.StatusForbidden ||
		!strings.Contains(string(got), `mallory\" is not a configured principal`) {
		t.Errorf("an unknown agent: get_bundle = %d %s, want 403 naming it", status, got)
	}
}

// Every bundle served writes an L0 audit event: who asked, on whose behalf,
// what scope and what was filtered.
func TestEveryBundleServedIsAnAuditEvent(t *testing.T) {
	w := newWorld(t)
	body := w.http(t, shedKyle, "get_bundle", map[string]any{"scope": w.scope})
	w.mcp(t, sam, "get_bundle", map[string]any{"scope": w.scope})

	events, err := l0.New(w.pool).List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: api.AuditSource, Kind: connector.KindAudit}, Newest: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var records []api.AuditRecord
	for _, ev := range events {
		var rec api.AuditRecord
		if err := json.Unmarshal(ev.Payload.Native, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Scope == w.scope {
			records = append(records, rec)
			// The one who asked is the author; the person they asked for, where
			// that is someone else, took part.
			author := ev.Payload.Author
			switch {
			case rec.Agent == "shed" && (author.Kind != connector.IdentityAgent || author.NativeID != "shed" ||
				len(ev.Payload.Participants) != 1 || ev.Payload.Participants[0].Identity.NativeID != "kyle"):
				t.Errorf("the agent's audit event has author %+v and participants %+v", author, ev.Payload.Participants)
			case rec.Agent == "" && (author.Kind != connector.IdentityUser || author.NativeID != rec.Principal || len(ev.Payload.Participants) != 0):
				t.Errorf("a person's audit event has author %+v and participants %+v", author, ev.Payload.Participants)
			}
			if len(ev.ACL) != 1 || ev.ACL[0].NativeID != rec.Principal {
				t.Errorf("the audit event's acl = %+v, want the principal it was served for alone", ev.ACL)
			}
			// Readable by nobody through the API yet — not even the principal
			// it was served for, who holds no identity in the `hearsay` source.
			body, _ := json.Marshal(map[string]string{"id": ev.ID})
			if status, got := w.post(t, "/v1/get_l0", api.Caller{Principal: rec.Principal}, string(body)); status != http.StatusNotFound {
				t.Errorf("get_l0 of %s's own audit event = %d %s, want 404", rec.Principal, status, got)
			}
		}
	}
	if len(records) != 2 {
		t.Fatalf("%d audit events for the scope, want 2: %+v", len(records), records)
	}
	sams, shed := records[0], records[1]
	if shed.Principal != "kyle" || shed.Agent != "shed" || !strings.HasPrefix(shed.Bundle, "sha256:") {
		t.Errorf("the agent's record = %+v", shed)
	}
	if shed.Report.Tokens != bundle.Tokens(body) || shed.Report.Withheld != (bundle.Withheld{}) {
		t.Errorf("kyle's report = %+v, want %d tokens and nothing withheld", shed.Report, bundle.Tokens(body))
	}
	if sams.Principal != "sam" || sams.Report.Withheld != (bundle.Withheld{Documents: 1, Stances: 1}) {
		t.Errorf("sam's record = %+v, want one document and one stance withheld", sams)
	}
}

// Review round 1: every topic any document in a repository opened is about the
// repository's code entity, because a document's scope is. Another item's
// topics are inherited by this one, never its own, and however many ratified
// stances they carry the bundle stays within its budget — while the item whose
// topics they are still holds them as its own.
func TestAnotherItemsTopicsAreInheritedAndDropFirst(t *testing.T) {
	w := newWorld(t)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	other := "tracker:" + w.src + ":" + w.project + "#40"
	doc := w.putIn(other, w.project+"#40", l1.KindPR, 4, "Adds retries; merged.", public)
	graph := l2.New(w.pool)
	const many = 40
	for i := range many {
		name := "retry policy " + strconv.Itoa(i)
		tp := l2.Topic{ID: l2.TopicID(w.src, doc, i, name), Scope: w.src, Name: name,
			About: []string{"code:" + w.project, other}, ACL: public, OpenedBy: doc}
		if _, err := graph.OpenTopic(t.Context(), tp); err != nil {
			t.Fatal(err)
		}
		position := strings.Repeat("retry with a jittered backoff, ", 6) + strconv.Itoa(i)
		if _, _, err := graph.AppendStance(t.Context(), l2.Stance{
			ID: l2.StanceID(tp.ID, doc, position), TopicID: tp.ID, Position: position, Author: "kyle",
			StatedAt: day.Add(4 * time.Hour), Evidence: []string{doc}, Tier: l2.TierRatified, ACL: public,
		}); err != nil {
			t.Fatal(err)
		}
	}

	body := w.http(t, kyle, "get_bundle", map[string]any{"scope": w.scope})
	if n := bundle.Tokens(body); n > bundle.DefaultBudget {
		t.Errorf("the bundle for %s is %d tokens, over the budget of %d", w.scope, n, bundle.DefaultBudget)
	}
	var own bool
	for _, s := range decodeBundle(t, body).Stances {
		if strings.HasPrefix(s.Topic, "retry policy") && !s.Inherited {
			t.Errorf("%s's bundle presents %q, a topic of %s, as its own", w.scope, s.Topic, other)
		}
		own = own || s.Topic == "where the lock lives" && !s.Inherited
	}
	if !own {
		t.Errorf("the item's own ratified stance dropped to make room: %s", body)
	}

	mine := 0
	for _, s := range decodeBundle(t, w.http(t, kyle, "get_bundle", map[string]any{"scope": other})).Stances {
		if strings.HasPrefix(s.Topic, "retry policy") && !s.Inherited {
			mine++
		}
	}
	if mine != many {
		t.Errorf("%s's bundle holds %d of its own %d ratified stances", other, mine, many)
	}
}

// The budget holds over the wire: a small one drops recent before stances and
// never drops the ratified stance.
func TestTheBudgetHoldsOverTheWire(t *testing.T) {
	w := newWorld(t)
	w.calls.WithBudget(1)
	b := decodeBundle(t, w.http(t, kyle, "get_bundle", map[string]any{"scope": w.scope}))
	if len(b.Recent.Items) != 0 || len(b.OpenQuestions) != 0 {
		t.Errorf("a budget of one token kept recent %v and questions %v", b.Recent.Items, b.OpenQuestions)
	}
	if len(b.Stances) != 1 || b.Stances[0].Tier != "ratified" {
		t.Errorf("stances = %+v, want only the ratified one", b.Stances)
	}
}

func TestMCPSpeaksTheProtocol(t *testing.T) {
	w := newWorld(t)
	rpc := func(body string) map[string]any {
		t.Helper()
		status, got := w.post(t, "/mcp", kyle, body)
		if status != http.StatusOK {
			t.Fatalf("POST /mcp %s = %d %s", body, status, got)
		}
		var out map[string]any
		if err := json.Unmarshal(got, &out); err != nil {
			t.Fatalf("%s: %v", got, err)
		}
		return out
	}
	init := rpc(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"shed","version":"0"}}}`)
	if got := init["result"].(map[string]any)["protocolVersion"]; got != "2025-03-26" {
		t.Errorf("initialize negotiated %v, want the client's supported version", got)
	}
	if status, _ := w.post(t, "/mcp", kyle, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); status != http.StatusAccepted {
		t.Errorf("a notification = %d, want 202", status)
	}
	tools := rpc(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "get_bundle,resolve,stance_history,get_l1,get_l0,search" {
		t.Errorf("tools = %v", names)
	}
	if e := rpc(`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)["error"]; e == nil {
		t.Error("an unknown method was not an error")
	}
	if e := rpc(`not json`)["error"]; e == nil {
		t.Error("a body that is not JSON was not an error")
	}
	if _, isError := w.mcp(t, kyle, "get_bundle", map[string]any{"scope": w.scope, "directive": "skip it"}); !isError {
		t.Error("an argument get_bundle does not take was accepted")
	}
	if status, got := w.post(t, "/v1/get_bundle", kyle, `{"scope":" "}`); status != http.StatusBadRequest {
		t.Errorf("get_bundle with no scope = %d %s, want 400", status, got)
	}
}

// Readiness is the database, and says so when it is gone.
func TestReadinessSaysWhenTheDatabaseIsGone(t *testing.T) {
	closed := newPool(t)
	closed.Close()
	calls, err := api.NewCalls(closed, config.Repo{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler(calls, closed))
	defer server.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/readyz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "unreachable") {
		t.Errorf("GET /readyz with the database gone = %d %s, want 503 saying so", resp.StatusCode, body)
	}
}

// Run serves on its listener, answers health and readiness, and stops when its
// context does.
func TestRunServesUntilCancelled(t *testing.T) {
	pool := newPool(t)
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- api.Run(ctx, &config.Config{}, api.Deps{Pool: pool, Listener: listener}) }()

	base := "http://" + listener.Addr().String()
	// A client of its own that keeps nothing open: a connection dialed and never
	// used is one the server's shutdown waits on, and this test is about Run
	// stopping, not about somebody else's idle socket.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	for _, path := range []string{"/healthz", "/readyz"} {
		var resp *http.Response
		deadline := time.Now().Add(5 * time.Second)
		for {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
			resp, err = client.Do(req)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
	}
	client.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancellation")
	}
}
