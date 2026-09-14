package github_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
)

const (
	sourceID   = "github-acme"
	testToken  = "test-token"
	hookSecret = "hook-secret"
	sha1       = "c0ffee0000000000000000000000000000000001"
	sha2       = "decaf00000000000000000000000000000000002"
)

// wantBackfill is every native id a backfill of acme/api emits, in the order it
// emits them: issues, pull requests with their reviews, issue comments, review
// comments, commits. The pull request listed among the issues and the pending
// review are not in it.
var wantBackfill = []string{
	"acme/api#1@2026-09-02T11:00:00Z",
	"acme/api#3@2026-08-20T09:30:00Z",
	"acme/api#2@2026-09-04T12:00:00Z",
	"acme/api#2:review:77",
	"acme/api#3:comment:997@2026-08-15T10:00:00Z",
	"acme/api#1:comment:998@2026-09-01T12:30:00Z",
	"acme/api#2:comment:999@2026-09-03T09:00:00Z",
	"acme/api#2:comment:88@2026-09-04T10:05:00Z",
	"acme/api@" + sha1,
	"acme/api@" + sha2,
}

// liveHooks are the deliveries for the objects the backfill fixtures hold.
var liveHooks = []struct{ event, file string }{
	{"issues", "issues.edited"},
	{"issue_comment", "issue_comment.created"},
	{"pull_request", "pull_request.synchronize"},
	{"pull_request_review", "pull_request_review.submitted"},
	{"pull_request_review_comment", "pull_request_review_comment.created"},
	{"push", "push"},
}

// route is one canned REST answer: a fixture file, and the next page's link.
type route struct {
	file string
	next string
}

func defaultRoutes() map[string]route {
	return map[string]route{
		"/repos/acme/api/issues?direction=asc&per_page=100&sort=updated&state=all": {file: "issues-1.json", next: "/repositories/42/issues?page=2"},
		"/repositories/42/issues?page=2":                                           {file: "issues-2.json"},
		"/repos/acme/api/pulls?direction=asc&per_page=100&sort=updated&state=all":  {file: "pulls.json"},
		"/repos/acme/api/pulls/2/reviews?per_page=100":                             {file: "reviews-2.json", next: "/repositories/42/pulls/2/reviews?page=2"},
		"/repositories/42/pulls/2/reviews?page=2":                                  {file: "reviews-2-page2.json"},
		"/repos/acme/api/issues/comments?direction=asc&per_page=100&sort=updated":  {file: "issue-comments.json"},
		"/repos/acme/api/pulls/comments?direction=asc&per_page=100&sort=updated":   {file: "review-comments.json"},
		"/repos/acme/api/commits?per_page=100&sha=main":                            {file: "commits.json"},
		"/repos/acme/api/commits/" + sha1:                                          {file: "commit-c0ffee.json"},
		"/repos/acme/api/commits/" + sha2:                                          {file: "commit-decaf.json"},
	}
}

// fakeGitHub is GitHub's REST API as far as the fixtures go. Routes match on
// the path and the query without `since`, which the tests that care about it
// read from the recorded requests. Any repository resolves, with the
// visibility the test set; lists under acme/web are empty.
type fakeGitHub struct {
	t       *testing.T
	srv     *httptest.Server
	private atomic.Bool

	mu       sync.Mutex
	routes   map[string]route
	status   map[string]int
	requests []string
	// once answers the next request for a key with a status, then normally.
	once map[string]int
	// holds make requests for a key wait until released; reached records
	// that a request for a key has arrived.
	holds   map[string]chan struct{}
	reached map[string]bool
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{t: t, routes: defaultRoutes(), status: map[string]int{},
		once: map[string]int{}, holds: map[string]chan struct{}{}, reached: map[string]bool{}}
	gh.srv = httptest.NewServer(http.HandlerFunc(gh.serve))
	t.Cleanup(gh.srv.Close)
	return gh
}

func (gh *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		gh.t.Errorf("request %s carries Authorization %q, want the configured token", r.URL.Path, got)
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	q.Del("since")
	key := r.URL.Path
	if len(q) > 0 {
		key += "?" + q.Encode()
	}

	gh.mu.Lock()
	gh.requests = append(gh.requests, r.URL.RequestURI())
	status, failing := gh.status[key]
	rt, ok := gh.routes[key]
	if code, once := gh.once[key]; once {
		delete(gh.once, key)
		status, failing = code, true
	}
	gh.reached[key] = true
	hold := gh.holds[key]
	gh.mu.Unlock()
	if hold != nil {
		<-hold
	}

	switch repo, isRepo := strings.CutPrefix(r.URL.Path, "/repos/"); {
	case failing:
		http.Error(w, "failing on purpose", status)
	case isRepo && strings.Count(repo, "/") == 1:
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": repo, "private": gh.private.Load(), "default_branch": "main"})
	case !ok && strings.HasPrefix(r.URL.Path, "/repos/acme/web/"):
		_, _ = w.Write([]byte("[]"))
	case !ok:
		gh.t.Errorf("unexpected request %s", r.URL.RequestURI())
		http.NotFound(w, r)
	default:
		if rt.next != "" {
			next := rt.next
			if !strings.HasPrefix(next, "http") {
				next = gh.srv.URL + next
			}
			// rel="last" first, so the parser has to find rel="next".
			w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/42/issues?page=9>; rel="last", <%s>; rel="next"`, gh.srv.URL, next))
		}
		body, err := os.ReadFile(filepath.Join("testdata", "api", rt.file))
		if err != nil {
			gh.t.Errorf("reading fixture: %v", err)
			http.Error(w, "no fixture", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}
}

func (gh *fakeGitHub) setRoute(key string, rt route) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.routes[key] = rt
}

func (gh *fakeGitHub) fail(key string, status int) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.status[key] = status
}

func (gh *fakeGitHub) seen() []string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return append([]string(nil), gh.requests...)
}

func (gh *fakeGitHub) failNext(key string, status int) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.once[key] = status
}

// hold makes requests for key wait until release is called, which the test's
// cleanup also does, so the server is never closed with a request stuck.
func (gh *fakeGitHub) hold(key string) (release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	gh.mu.Lock()
	gh.holds[key] = ch
	gh.mu.Unlock()
	gh.t.Cleanup(release)
	return release
}

func (gh *fakeGitHub) wasReached(key string) bool {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return gh.reached[key]
}

// newSource is a source served by the fake, with settings merged over its URL.
func newSource(t *testing.T, gh *fakeGitHub, id string, settings map[string]any, repos ...string) connector.SourceConfig {
	t.Helper()
	merged := map[string]any{"api_url": gh.srv.URL}
	for k, v := range settings {
		merged[k] = v
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) == 0 {
		repos = []string{"acme/api"}
	}
	return connector.SourceConfig{
		ID:         id,
		Type:       github.Type,
		Containers: repos,
		Settings:   raw,
		Secrets:    map[string]string{github.SecretToken: testToken, github.SecretWebhook: hookSecret},
	}
}

func newConnector(t *testing.T, src connector.SourceConfig) *github.Connector {
	t.Helper()
	c, err := github.New(src)
	if err != nil {
		t.Fatalf("github.New = %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// gateFor is the sink production puts in front of the connector.
func gateFor(src connector.SourceConfig, c connector.Connector, sink connector.Sink) *connector.Gate {
	return connector.NewGate(sink, src.ID, c.Describe(), connector.NewAllowlist(src))
}

// backfillAll drives a backfill to the end and returns the cursors it went
// through.
func backfillAll(t *testing.T, c connector.Backfiller, sink connector.Sink) []connector.Cursor {
	t.Helper()
	var cursors []connector.Cursor
	var cur connector.Cursor
	for range 100 {
		res, err := c.Backfill(t.Context(), sink, cur)
		if err != nil {
			t.Fatalf("Backfill(%q) = %v", cur, err)
		}
		if res.Done {
			return cursors
		}
		if res.Next == cur {
			t.Fatalf("Backfill(%q) made no progress", cur)
		}
		cur = res.Next
		cursors = append(cursors, cur)
	}
	t.Fatal("backfill did not finish in 100 calls")
	return nil
}

func hook(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "hooks", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// deliver posts a delivery signed with the source's secret.
func deliver(t *testing.T, h http.Handler, event string, body []byte) int {
	t.Helper()
	return deliverSigned(t, h, event, body, sign(hookSecret, body))
}

func deliverSigned(t *testing.T, h http.Handler, event string, body []byte, signature string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/hooks/"+sourceID, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "d-1")
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func nativeIDs(events []connector.Event) []string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.NativeID)
	}
	return ids
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFactory(t *testing.T) {
	gh := newFakeGitHub(t)
	tests := []struct {
		name    string
		mutate  func(*connector.SourceConfig)
		wantErr string
	}{
		{name: "a complete source builds"},
		{name: "a date for since", mutate: settings(`{"since":"2026-01-01"}`)},
		{name: "a timestamp for since", mutate: settings(`{"since":"2026-01-01T10:00:00+02:00"}`)},
		{name: "no token", mutate: func(s *connector.SourceConfig) { delete(s.Secrets, "token") }, wantErr: `secret "token" is required`},
		{name: "no webhook secret", mutate: func(s *connector.SourceConfig) { delete(s.Secrets, "webhook_secret") }, wantErr: `secret "webhook_secret" is required`},
		{name: "a secret it does not read", mutate: func(s *connector.SourceConfig) { s.Secrets["app_key"] = "x" }, wantErr: `secret "app_key" is not one`},
		{name: "no containers", mutate: func(s *connector.SourceConfig) { s.Containers = nil }, wantErr: "containers is empty"},
		{name: "every container", mutate: func(s *connector.SourceConfig) { s.Containers = []string{"*"} }, wantErr: `containers "*" is not supported`},
		{name: "an owner alone", mutate: func(s *connector.SourceConfig) { s.Containers = []string{"acme"} }, wantErr: "not a repository full name"},
		{name: "an empty owner", mutate: func(s *connector.SourceConfig) { s.Containers = []string{"/api"} }, wantErr: "not a repository full name"},
		{name: "an empty name", mutate: func(s *connector.SourceConfig) { s.Containers = []string{"acme/"} }, wantErr: "not a repository full name"},
		{name: "a path", mutate: func(s *connector.SourceConfig) { s.Containers = []string{"acme/api/x"} }, wantErr: "not a repository full name"},
		{name: "since that is not a date", mutate: settings(`{"since":"yesterday"}`), wantErr: `settings.since "yesterday"`},
		{name: "an api_url that is not http", mutate: settings(`{"api_url":"ftp://example.com"}`), wantErr: "settings.api_url"},
		{name: "an api_url with no host", mutate: settings(`{"api_url":"https://"}`), wantErr: "settings.api_url"},
		{name: "an api_url with a query", mutate: settings(`{"api_url":"https://example.com/?a=b"}`), wantErr: "settings.api_url"},
		{name: "a setting it does not have", mutate: settings(`{"include_forks":false}`), wantErr: "include_forks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource(t, gh, sourceID, nil)
			if tt.mutate != nil {
				tt.mutate(&src)
			}
			registry := connector.NewRegistry()
			if err := registry.Register(github.Type, github.Factory); err != nil {
				t.Fatal(err)
			}
			c, err := registry.New(t.Context(), src)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("New = %v, want a connector", err)
				}
				t.Cleanup(func() { _ = c.Close(context.Background()) })
				if _, ok := c.(connector.Pusher); !ok {
					t.Error("the connector is not a Pusher")
				}
				if _, ok := c.(connector.Backfiller); !ok {
					t.Error("the connector is not a Backfiller")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func settings(raw string) func(*connector.SourceConfig) {
	return func(s *connector.SourceConfig) { s.Settings = json.RawMessage(raw) }
}

func TestBackfillEmitsTheContractsEvents(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))

	events := rec.Events()
	if got := nativeIDs(events); asJSON(t, got) != asJSON(t, wantBackfill) {
		t.Fatalf("native ids =\n%v\nwant\n%v", got, wantBackfill)
	}

	kyle := connector.Identity{Source: sourceID, Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjE=", Handle: "kpenfound"}
	robin := connector.Identity{Source: sourceID, Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjI=", Handle: "robinok"}
	shed := connector.Identity{Source: sourceID, Kind: connector.IdentityBot, NativeID: "BOT_kgDOAAAAAw", Handle: "shed-agent[bot]"}
	committer := kyle
	committer.DisplayName, committer.Email = "Kyle Penfound", "kyle@example.com"
	ghost := connector.Identity{Source: sourceID, Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjEwMTM3", Handle: "ghost", DisplayName: "Pat Example", Email: "pat@example.com"}

	tests := []struct {
		nativeID string
		kind     connector.Kind
		artifact string
		parent   string
		author   connector.Identity
		title    string
		text     string
		time     string
		editedAt string
		native   string
	}{
		{
			nativeID: wantBackfill[0], kind: connector.KindIssue, artifact: "acme/api#1", author: kyle,
			title: "/health returns 500", text: "The API returns 500 on /health when the database is up.",
			time: "2026-09-01T10:00:00Z", editedAt: "2026-09-02T11:00:00Z", native: `"labels":["bug"]`,
		},
		{
			nativeID: wantBackfill[1], kind: connector.KindIssue, artifact: "acme/api#3", author: robin,
			title: "Document the deploy job", time: "2026-08-01T09:00:00Z", editedAt: "2026-08-20T09:30:00Z",
			native: `"closed_at":"2026-08-20T09:30:00Z"`,
		},
		{
			nativeID: wantBackfill[2], kind: connector.KindPullRequest, artifact: "acme/api#2", author: robin,
			title: "Fix /health", text: "Fixes #1", time: "2026-09-03T08:00:00Z", editedAt: "2026-09-04T12:00:00Z",
			native: `"head":{"ref":"fix-health"`,
		},
		{
			nativeID: wantBackfill[3], kind: connector.KindReview, artifact: "acme/api#2:review:77", parent: "acme/api#2",
			author: kyle, text: "Looks good.", time: "2026-09-04T11:00:00Z", native: `"state":"approved"`,
		},
		{
			nativeID: wantBackfill[4], kind: connector.KindMessage, artifact: "acme/api#3:comment:997", parent: "acme/api#3",
			author: kyle, text: "Done in the runbook.", time: "2026-08-15T10:00:00Z", editedAt: "2026-08-15T10:00:00Z",
		},
		{
			nativeID: wantBackfill[5], kind: connector.KindMessage, artifact: "acme/api#1:comment:998", parent: "acme/api#1",
			author: robin, text: "Seeing this too.", time: "2026-09-01T12:00:00Z", editedAt: "2026-09-01T12:30:00Z",
		},
		{
			nativeID: wantBackfill[6], kind: connector.KindMessage, artifact: "acme/api#2:comment:999", parent: "acme/api#2",
			author: shed, text: "CI passed.", time: "2026-09-03T09:00:00Z", editedAt: "2026-09-03T09:00:00Z",
		},
		{
			nativeID: wantBackfill[7], kind: connector.KindReviewComment, artifact: "acme/api#2:comment:88", parent: "acme/api#2",
			author: kyle, text: "Nit: name this handler.", time: "2026-09-04T10:00:00Z", editedAt: "2026-09-04T10:05:00Z",
			native: `"path":"api/health.go"`,
		},
		{
			nativeID: wantBackfill[8], kind: connector.KindCommit, artifact: "acme/api@" + sha1, author: committer,
			title: "Fix /health (#2)", text: "Fix /health (#2)\n\nReturn 200 when the database is up.",
			time: "2026-09-04T13:05:00Z", native: `"parents":["` + sha2 + `"]`,
		},
		{
			nativeID: wantBackfill[9], kind: connector.KindCommit, artifact: "acme/api@" + sha2, author: ghost,
			title: "Initial commit", text: "Initial commit", time: "2026-08-01T08:00:00Z",
		},
	}
	for i, tt := range tests {
		t.Run(tt.nativeID, func(t *testing.T) {
			ev := events[i]
			p := ev.Payload
			if ev.ID != connector.EventID(sourceID, tt.nativeID) {
				t.Errorf("id = %q", ev.ID)
			}
			if ev.Kind != tt.kind || p.Artifact != tt.artifact {
				t.Errorf("kind, artifact = %q, %q; want %q, %q", ev.Kind, p.Artifact, tt.kind, tt.artifact)
			}
			if p.Parent != tt.parent || p.Thread != tt.parent {
				t.Errorf("parent, thread = %q, %q; want %q for both", p.Parent, p.Thread, tt.parent)
			}
			if p.Author == nil || *p.Author != tt.author {
				t.Errorf("author = %+v, want %+v", p.Author, tt.author)
			}
			if p.Title != tt.title || p.Text != tt.text {
				t.Errorf("title, text = %q, %q; want %q, %q", p.Title, p.Text, tt.title, tt.text)
			}
			if got := ev.Time.Format(time.RFC3339); got != tt.time {
				t.Errorf("time = %s, want %s", got, tt.time)
			}
			switch {
			case tt.editedAt == "" && p.Revision != nil:
				t.Errorf("revision = %+v, want none", p.Revision)
			case tt.editedAt != "" && p.Revision == nil:
				t.Errorf("revision is absent, want one edited at %s", tt.editedAt)
			case tt.editedAt != "":
				if p.Revision.Token != tt.editedAt || p.Revision.EditedAt.Format(time.RFC3339) != tt.editedAt {
					t.Errorf("revision = %+v, want token and edited_at %s", p.Revision, tt.editedAt)
				}
			}
			if !strings.Contains(string(p.Native), tt.native) {
				t.Errorf("native = %s, want it to contain %s", p.Native, tt.native)
			}
			if p.URL == "" {
				t.Error("url is empty")
			}
			wantContainer := connector.Container{Kind: connector.ContainerRepository, NativeID: "acme/api", Name: "acme/api"}
			if p.Container != wantContainer {
				t.Errorf("container = %+v, want %+v", p.Container, wantContainer)
			}
			if asJSON(t, ev.ACL) != `[{"kind":"public"}]` {
				t.Errorf("acl = %s, want public", asJSON(t, ev.ACL))
			}
		})
	}
}

// Backfilling the same repository twice emits the same events, so a second
// backfill writes nothing new.
func TestBackfillTwiceEmitsTheSameEvents(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	first, second := &connector.Recorder{}, &connector.Recorder{}
	c := newConnector(t, src)
	backfillAll(t, c, gateFor(src, c, first))
	again := newConnector(t, src)
	backfillAll(t, again, gateFor(src, again, second))

	if a, b := asJSON(t, first.Events()), asJSON(t, second.Events()); a != b {
		t.Errorf("the second backfill emitted\n%s\nthe first\n%s", b, a)
	}
}

// A cursor is all a restarted process has: a connector built fresh for every
// call walks the same history.
func TestBackfillResumesFromItsCursorInANewConnector(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	whole := &connector.Recorder{}
	c := newConnector(t, src)
	cursors := backfillAll(t, c, gateFor(src, c, whole))

	pieces := &connector.Recorder{}
	var cur connector.Cursor
	for {
		fresh := newConnector(t, src)
		res, err := fresh.Backfill(t.Context(), gateFor(src, fresh, pieces), cur)
		if err != nil {
			t.Fatalf("Backfill(%q) = %v", cur, err)
		}
		if res.Done {
			break
		}
		cur = res.Next
	}
	if a, b := asJSON(t, whole.Events()), asJSON(t, pieces.Events()); a != b {
		t.Errorf("resumed backfill emitted\n%s\nwant\n%s", b, a)
	}
	for _, cur := range cursors {
		if len(cur) > connector.MaxCursorLen || !utf8.ValidString(string(cur)) || strings.ContainsRune(string(cur), 0) {
			t.Errorf("cursor %q is not one the runtime can store", cur)
		}
		if strings.Contains(string(cur), gh.srv.URL) {
			t.Errorf("cursor %q carries the API host", cur)
		}
	}
}

func TestBackfillFromCursor(t *testing.T) {
	tests := []struct {
		name       string
		cursor     string
		wantDone   bool
		wantEvents int
		wantNext   string
		wantErr    string
	}{
		{
			name:       "a removed repository before the configured one resumes at it",
			cursor:     `{"repo":"acme/aaa","step":"commits"}`,
			wantEvents: 1, // issue #1; the pull request on the page is not an issue
			wantNext:   `{"repo":"acme/api","step":"issues","next":"/repositories/42/issues?page=2"}`,
		},
		{
			name:     "a removed repository after the last one is the end",
			cursor:   `{"repo":"acme/zzz","step":"issues"}`,
			wantDone: true,
		},
		{
			name:       "the last step of the last repository is the end",
			cursor:     `{"repo":"acme/api","step":"commits"}`,
			wantDone:   true,
			wantEvents: 2,
		},
		{
			name:       "a step with no more pages moves to the next step",
			cursor:     `{"repo":"acme/api","step":"review_comments"}`,
			wantEvents: 1,
			wantNext:   `{"repo":"acme/api","step":"commits"}`,
		},
		{name: "a cursor that is not JSON", cursor: "3", wantErr: "not one the github connector wrote"},
		{name: "a step it does not have", cursor: `{"repo":"acme/api","step":"labels"}`, wantErr: `step "labels"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			rec := &connector.Recorder{}
			res, err := c.Backfill(t.Context(), gateFor(src, c, rec), connector.Cursor(tt.cursor))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Backfill = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Backfill = %v", err)
			}
			if res.Done != tt.wantDone || res.Events != tt.wantEvents || len(rec.Events()) != tt.wantEvents {
				t.Errorf("Backfill = %+v with %d events recorded, want done %v and %d events", res, len(rec.Events()), tt.wantDone, tt.wantEvents)
			}
			if !tt.wantDone && string(res.Next) != tt.wantNext {
				t.Errorf("Next = %s, want %s", res.Next, tt.wantNext)
			}
		})
	}
}

func TestBackfillWalksEveryRepository(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil, "acme/web", "acme/api")
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))

	if got := nativeIDs(rec.Events()); asJSON(t, got) != asJSON(t, wantBackfill) {
		t.Errorf("native ids = %v, want %v", got, wantBackfill)
	}
	seen := gh.seen()
	if last := seen[len(seen)-1]; last != "/repos/acme/web/commits?per_page=100&sha=main" {
		t.Errorf("last request = %s, want acme/web's commits after acme/api's walk", last)
	}
}

func TestBackfillSince(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, map[string]any{"since": "2026-09-01"})
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))

	var want []string
	for _, id := range wantBackfill {
		if !strings.HasPrefix(id, "acme/api#3") {
			want = append(want, id)
		}
	}
	if got := nativeIDs(rec.Events()); asJSON(t, got) != asJSON(t, want) {
		t.Errorf("native ids = %v, want %v", got, want)
	}

	for _, uri := range gh.seen() {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		since := u.Query().Get("since")
		switch {
		case strings.HasPrefix(u.Path, "/repositories/"):
			// A next-page link is GitHub's, query and all.
		case strings.HasSuffix(u.Path, "/issues"), strings.HasSuffix(u.Path, "/comments"), strings.HasSuffix(u.Path, "/commits"):
			if since != "2026-09-01T00:00:00Z" {
				t.Errorf("%s: since = %q, want the configured start date", uri, since)
			}
		case strings.HasSuffix(u.Path, "/pulls"):
			// The pulls list takes no since; the connector filters the page.
			if since != "" {
				t.Errorf("%s: since = %q, want none", uri, since)
			}
		}
	}
}

func TestBackfillFailures(t *testing.T) {
	tests := []struct {
		name    string
		cursor  string
		setup   func(*fakeGitHub)
		wantErr string
	}{
		{
			name:    "the repository cannot be read",
			setup:   func(gh *fakeGitHub) { gh.fail("/repos/acme/api", http.StatusNotFound) },
			wantErr: "404",
		},
		{
			name: "a list page fails",
			setup: func(gh *fakeGitHub) {
				gh.fail("/repos/acme/api/issues?direction=asc&per_page=100&sort=updated&state=all", http.StatusBadGateway)
			},
			wantErr: "502",
		},
		{
			name: "a next-page link to another host",
			setup: func(gh *fakeGitHub) {
				gh.setRoute("/repos/acme/api/issues?direction=asc&per_page=100&sort=updated&state=all",
					route{file: "issues-1.json", next: "http://elsewhere.example/repositories/42/issues?page=2"})
			},
			wantErr: "outside the configured API",
		},
		{
			name:   "a review page fails after its pull request was emitted",
			cursor: `{"repo":"acme/api","step":"pulls"}`,
			setup: func(gh *fakeGitHub) {
				gh.fail("/repos/acme/api/pulls/2/reviews?per_page=100", http.StatusInternalServerError)
			},
			wantErr: "500",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			tt.setup(gh)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			res, err := c.Backfill(t.Context(), gateFor(src, c, &connector.Recorder{}), connector.Cursor(tt.cursor))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Backfill = %+v, %v; want an error containing %q", res, err, tt.wantErr)
			}
		})
	}
}

// GitHub answers 409 for the commits of a repository with nothing in it.
func TestBackfillOfAnEmptyRepositoryFinishes(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.fail("/repos/acme/api/commits?per_page=100&sha=main", http.StatusConflict)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	res, err := c.Backfill(t.Context(), gateFor(src, c, &connector.Recorder{}), `{"repo":"acme/api","step":"commits"}`)
	if err != nil || !res.Done || res.Events != 0 {
		t.Errorf("Backfill = %+v, %v; want done with nothing", res, err)
	}
}

// assertPrivateOf checks that private is public re-observed in a private
// repository: the same artifacts in the same order, the composed token, the
// collaborator group, and nothing else changed.
func assertPrivateOf(t *testing.T, public, private []connector.Event) {
	t.Helper()
	if len(private) != len(public) {
		t.Fatalf("%d private events, want one for each of the %d public ones:\n%v", len(private), len(public), nativeIDs(private))
	}
	wantACL := `[{"kind":"group","source":"` + sourceID + `","native_id":"acme/api"}]`
	for i, pub := range public {
		got := private[i]
		wantToken := "perm:private"
		var wantEditedAt time.Time
		if pub.Payload.Revision != nil {
			wantToken = pub.Payload.Revision.Token + "+perm:private"
			wantEditedAt = pub.Payload.Revision.EditedAt
		}
		if want := pub.Payload.Artifact + "@" + wantToken; got.NativeID != want {
			t.Errorf("native id = %s, want %s", got.NativeID, want)
			continue
		}
		if got.Payload.Revision == nil || got.Payload.Revision.Token != wantToken || !got.Payload.Revision.EditedAt.Equal(wantEditedAt) {
			t.Errorf("%s: revision = %+v, want token %s edited at %v", got.NativeID, got.Payload.Revision, wantToken, wantEditedAt)
		}
		if a := asJSON(t, got.ACL); a != wantACL {
			t.Errorf("%s: acl = %s, want %s", got.NativeID, a, wantACL)
		}
		pubPayload, gotPayload := pub.Payload, got.Payload
		pubPayload.Revision, gotPayload.Revision = nil, nil
		if a, b := asJSON(t, pubPayload), asJSON(t, gotPayload); a != b || !pub.Time.Equal(got.Time) || pub.Kind != got.Kind {
			t.Errorf("%s: content changed:\n%s\nwas\n%s", got.NativeID, b, a)
		}
	}
}

func TestBackfillOfAPrivateRepository(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	public := &connector.Recorder{}
	c := newConnector(t, src)
	backfillAll(t, c, gateFor(src, c, public))

	gh.private.Store(true)
	private := &connector.Recorder{}
	again := newConnector(t, src)
	backfillAll(t, again, gateFor(src, again, private))
	assertPrivateOf(t, public.Events(), private.Events())
}

func TestWebhookSignature(t *testing.T) {
	body := hook(t, "issues.edited")
	tampered := bytes.Replace(body, []byte("when the database is up"), []byte("always"), 1)
	tests := []struct {
		name      string
		method    string
		body      []byte
		signature string
		wantCode  int
	}{
		{name: "a valid signature is ingested", body: body, signature: sign(hookSecret, body), wantCode: http.StatusAccepted},
		{name: "no signature", body: body, wantCode: http.StatusUnauthorized},
		{name: "signed with another secret", body: body, signature: sign("not-the-secret", body), wantCode: http.StatusUnauthorized},
		{name: "a signature over another body", body: tampered, signature: sign(hookSecret, body), wantCode: http.StatusUnauthorized},
		{name: "the sha1 header scheme", body: body, signature: "sha1=" + strings.TrimPrefix(sign(hookSecret, body), "sha256="), wantCode: http.StatusUnauthorized},
		{name: "a signature that is not hex", body: body, signature: "sha256=zz", wantCode: http.StatusUnauthorized},
		{name: "an empty signature", body: body, signature: "sha256=", wantCode: http.StatusUnauthorized},
		{name: "a valid digest with no scheme", body: body, signature: strings.TrimPrefix(sign(hookSecret, body), "sha256="), wantCode: http.StatusUnauthorized},
		{name: "not a POST", method: http.MethodGet, body: body, signature: sign(hookSecret, body), wantCode: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			rec := &connector.Recorder{}
			h := c.Handler(gateFor(src, c, rec))

			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			req := httptest.NewRequestWithContext(t.Context(), method, "/hooks/"+sourceID, bytes.NewReader(tt.body))
			req.Header.Set("X-GitHub-Event", "issues")
			if tt.signature != "" {
				req.Header.Set("X-Hub-Signature-256", tt.signature)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rr.Code, tt.wantCode)
			}
			wantEvents := 0
			if tt.wantCode == http.StatusAccepted {
				wantEvents = 1
			}
			if got := len(rec.Events()); got != wantEvents {
				t.Errorf("%d events emitted, want %d", got, wantEvents)
			}
		})
	}
}

// Webhook events for the objects a backfill read are the same events, so
// ingest deduplicates them — in a public repository and in a private one.
func TestWebhookEventsEqualBackfilledEvents(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprintf("private=%v", private), func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.private.Store(private)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			backfilled := &connector.Recorder{}
			backfillAll(t, c, gateFor(src, c, backfilled))
			byID := map[string]connector.Event{}
			for _, ev := range backfilled.Events() {
				byID[ev.ID] = ev
			}

			live := &connector.Recorder{}
			h := c.Handler(gateFor(src, c, live))
			for _, d := range liveHooks {
				body := hook(t, d.file)
				if private {
					body = bytes.ReplaceAll(body, []byte(`"private": false`), []byte(`"private": true`))
				}
				if code := deliver(t, h, d.event, body); code != http.StatusAccepted {
					t.Errorf("%s: status = %d, want 202", d.file, code)
				}
			}

			events := live.Events()
			if len(events) != 7 {
				t.Errorf("webhooks emitted %d events, want 7: %v", len(events), nativeIDs(events))
			}
			for _, ev := range events {
				want, ok := byID[ev.ID]
				if !ok {
					t.Errorf("webhook emitted %s, which the backfill did not", ev.NativeID)
					continue
				}
				if a, b := asJSON(t, ev), asJSON(t, want); a != b {
					t.Errorf("webhook event differs from the backfilled one:\n%s\nwant\n%s", a, b)
				}
			}
		})
	}
}

func TestWebhookTombstones(t *testing.T) {
	tests := []struct {
		event, file string
		wantTarget  string
		wantTime    string
	}{
		{"issue_comment", "issue_comment.deleted", "acme/api#1:comment:998", "2026-09-01T12:30:00Z"},
		{"pull_request_review_comment", "pull_request_review_comment.deleted", "acme/api#2:comment:88", "2026-09-04T10:05:00Z"},
		{"issues", "issues.deleted", "acme/api#3", "2026-08-20T09:30:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			rec := &connector.Recorder{}
			h := c.Handler(gateFor(src, c, rec))
			body := hook(t, tt.file)
			// Delivered twice, as a redelivery would: one event, twice.
			for range 2 {
				if code := deliver(t, h, tt.event, body); code != http.StatusAccepted {
					t.Fatalf("status = %d, want 202", code)
				}
			}
			events := rec.Events()
			if len(events) != 2 || asJSON(t, events[0]) != asJSON(t, events[1]) {
				t.Fatalf("events = %s, want the same tombstone twice", asJSON(t, events))
			}
			ev := events[0]
			if ev.Kind != connector.KindTombstone || ev.Payload.Target != tt.wantTarget || ev.Payload.Artifact != tt.wantTarget+":tombstone" || ev.NativeID != ev.Payload.Artifact {
				t.Errorf("tombstone = %s", asJSON(t, ev))
			}
			if got := ev.Time.Format(time.RFC3339); got != tt.wantTime {
				t.Errorf("time = %s, want %s", got, tt.wantTime)
			}
			if asJSON(t, ev.ACL) != `[{"kind":"public"}]` {
				t.Errorf("acl = %s, want the retracted artifact's", asJSON(t, ev.ACL))
			}
		})
	}
}

func TestWebhookIgnoresWhatItDoesNotIngest(t *testing.T) {
	repo := `"repository":{"full_name":"acme/api","private":false,"default_branch":"main"}`
	review := strings.Replace(string(hook(t, "pull_request_review.submitted")), `"action": "submitted"`, `"action": "dismissed"`, 1)
	pending := strings.Replace(string(hook(t, "pull_request_review.submitted")), `"state": "approved"`, `"state": "pending"`, 1)
	tests := []struct {
		name, event, body string
	}{
		{"a ping", "ping", `{"zen":"Keep it logically awesome.","hook_id":1}`},
		{"a push to another branch", "push", `{"ref":"refs/heads/feature","commits":[{"id":"` + sha1 + `"}],` + repo + `}`},
		{"a push that deletes the default branch", "push", `{"ref":"refs/heads/main","deleted":true,"commits":[],` + repo + `}`},
		{"a tag push", "push", `{"ref":"refs/tags/main","commits":[{"id":"` + sha1 + `"}],` + repo + `}`},
		{"a dismissed review", "pull_request_review", review},
		{"a pending review", "pull_request_review", pending},
		{"an event it does not read", "star", `{"action":"created",` + repo + `}`},
		{"an organisation event with no repository", "organization", `{"action":"member_added"}`},
		{"a repository going public", "repository", `{"action":"publicized",` + repo + `}`},
		{"another repository going private", "repository", `{"action":"privatized","repository":{"full_name":"acme/other","private":true}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			rec := &connector.Recorder{}
			if code := deliver(t, c.Handler(gateFor(src, c, rec)), tt.event, []byte(tt.body)); code != http.StatusNoContent {
				t.Errorf("status = %d, want 204", code)
			}
			if err := c.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if n := len(rec.Events()); n != 0 {
				t.Errorf("%d events emitted, want none", n)
			}
			if seen := gh.seen(); len(seen) != 0 {
				t.Errorf("requests %v, want none", seen)
			}
		})
	}
}

func TestWebhookRefusesWhatItCannotRead(t *testing.T) {
	repo := `"repository":{"full_name":"acme/api","private":false,"default_branch":"main"}`
	tests := []struct {
		name, event, body string
	}{
		{"not JSON", "issues", `{"action":`},
		{"an issues event with no issue", "issues", `{"action":"opened",` + repo + `}`},
		{"a comment with no issue url", "issue_comment", `{"action":"created","comment":{"id":1,"body":"x","issue_url":"https://api.github.com/repos/acme/api/issues/"},` + repo + `}`},
		{"a deleted review comment with no pull request url", "pull_request_review_comment", `{"action":"deleted","comment":{"id":1},` + repo + `}`},
		{"a review with no pull request", "pull_request_review", `{"action":"submitted","review":{"id":1,"state":"approved","submitted_at":"2026-09-01T00:00:00Z"},` + repo + `}`},
		{"a push naming a commit with no id", "push", `{"ref":"refs/heads/main","commits":[{}],` + repo + `}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			rec := &connector.Recorder{}
			if code := deliver(t, c.Handler(gateFor(src, c, rec)), tt.event, []byte(tt.body)); code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", code)
			}
			if n := len(rec.Events()); n != 0 {
				t.Errorf("%d events emitted, want none", n)
			}
		})
	}
}

func TestWebhookFailuresAreServerErrors(t *testing.T) {
	tests := []struct {
		name  string
		event string
		file  string
		setup func(*fakeGitHub, *connector.Recorder)
	}{
		{
			name: "the store refuses", event: "issues", file: "issues.edited",
			setup: func(_ *fakeGitHub, rec *connector.Recorder) { rec.Err = errors.New("store is down") },
		},
		{
			name: "a pushed commit cannot be read", event: "push", file: "push",
			setup: func(gh *fakeGitHub, _ *connector.Recorder) {
				gh.fail("/repos/acme/api/commits/"+sha2, http.StatusInternalServerError)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			rec := &connector.Recorder{}
			tt.setup(gh, rec)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			if code := deliver(t, c.Handler(gateFor(src, c, rec)), tt.event, hook(t, tt.file)); code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", code)
			}
			if n := len(rec.Events()); n != 0 {
				t.Errorf("%d events emitted, want none", n)
			}
		})
	}
}

// GitHub's names are case-insensitive; the ids and the container use the
// configured spelling, so the allowlist admits the event and it equals the
// backfilled one.
func TestWebhookUsesTheConfiguredSpellingOfTheRepository(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	body := bytes.ReplaceAll(hook(t, "issues.edited"), []byte(`"full_name": "acme/api"`), []byte(`"full_name": "Acme/API"`))
	if code := deliver(t, c.Handler(gateFor(src, c, rec)), "issues", body); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	events := rec.Events()
	if len(events) != 1 || events[0].NativeID != wantBackfill[0] || events[0].Payload.Container.NativeID != "acme/api" {
		t.Errorf("events = %s, want %s in acme/api", asJSON(t, events), wantBackfill[0])
	}
}

// A repository going private re-emits every artifact with the composed token
// and the collaborator group, and nothing else about any of them changes.
func TestRepositoryGoingPrivateResyncsEveryArtifact(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	public := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, public))
	if h := c.Health(t.Context()); h.Status != connector.HealthOK || h.LastEventAt.IsZero() {
		t.Errorf("health = %+v, want ok with a last event", h)
	}

	gh.private.Store(true)
	resynced := &connector.Recorder{}
	if code := deliver(t, c.Handler(gateFor(src, c, resynced)), "repository", hook(t, "repository.privatized")); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	waitFor(t, "the re-sync", func() bool { return len(resynced.Events()) >= len(public.Events()) })
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	assertPrivateOf(t, public.Events(), resynced.Events())
}

// A re-sync that cannot read the repository reports degraded and retries; Close
// stops it rather than waiting out the retry.
func TestFailingResyncDegradesHealthAndCloseStopsIt(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.fail("/repos/acme/api", http.StatusInternalServerError)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	if code := deliver(t, c.Handler(gateFor(src, c, &connector.Recorder{})), "repository", hook(t, "repository.privatized")); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	waitFor(t, "degraded health", func() bool { return c.Health(t.Context()).Status == connector.HealthDegraded })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want the re-sync stopped", err)
	}
	if h := c.Health(t.Context()); h.Status != connector.HealthOK {
		t.Errorf("health after the re-sync stopped = %+v, want ok", h)
	}
}

// A re-sync that failed and then worked reports ok again while it is still
// walking, not only once it has finished.
func TestResyncRecoversFromAFailure(t *testing.T) {
	t.Cleanup(github.SetResyncRetry(time.Millisecond))
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	gh.failNext("/repos/acme/api", http.StatusBadGateway)
	commits := "/repos/acme/api/commits?per_page=100&sha=main"
	release := gh.hold(commits)

	if code := deliver(t, c.Handler(gateFor(src, c, rec)), "repository", hook(t, "repository.privatized")); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	waitFor(t, "the re-sync to reach the commits", func() bool { return gh.wasReached(commits) })
	if h := c.Health(t.Context()); h.Status != connector.HealthOK {
		t.Errorf("health of a re-sync that recovered = %+v, want ok", h)
	}
	release()
	waitFor(t, "the re-sync", func() bool { return len(rec.Events()) >= len(wantBackfill) })
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if n := len(rec.Events()); n != len(wantBackfill) {
		t.Errorf("re-sync emitted %d events, want %d", n, len(wantBackfill))
	}
}

// A second delivery of the same visibility change while its re-sync runs
// starts no second walk, and a re-sync reads its own repository and no other.
func TestResyncRunsOncePerRepositoryAndStaysInIt(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil, "acme/api", "acme/web")
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	h := c.Handler(gateFor(src, c, rec))
	issues := "/repos/acme/api/issues?direction=asc&per_page=100&sort=updated&state=all"
	release := gh.hold(issues)

	if code := deliver(t, h, "repository", hook(t, "repository.privatized")); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	waitFor(t, "the re-sync to start", func() bool { return gh.wasReached(issues) })
	if code := deliver(t, h, "repository", hook(t, "repository.privatized")); code != http.StatusNoContent {
		t.Fatalf("redelivery: status = %d, want 204: a re-sync of the repository is already running", code)
	}
	release()
	waitFor(t, "the re-sync", func() bool { return len(rec.Events()) >= len(wantBackfill) })
	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if n := len(rec.Events()); n != len(wantBackfill) {
		t.Errorf("re-sync emitted %d events, want %d: the redelivery started a second walk", n, len(wantBackfill))
	}
	for _, uri := range gh.seen() {
		if strings.HasPrefix(uri, "/repos/acme/web") {
			t.Errorf("the re-sync of acme/api read %s", uri)
		}
	}
}
