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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

const (
	sourceID    = "github-acme"
	testToken   = "test-token"
	hookSecret  = "hook-secret"
	sha1        = "c0ffee0000000000000000000000000000000001"
	sha2        = "decaf00000000000000000000000000000000002"
	baseSHA     = "a100000000000000000000000000000000000000"
	nullSHA     = "0000000000000000000000000000000000000000"
	reviewToken = "cca24e8b92a08e66"
	pullToken   = "2026-09-04T12:00:00Z+paths:83f03ef649440f8e"
)

var pullPaths = []string{"api/health.go", "docs/health.md", "engine/server/health.go", "engine/server/x.go"}

var wantBackfill = []string{
	"acme/api#3@2026-08-20T09:30:00Z",
	"acme/api#1@2026-09-02T11:00:00Z",
	"acme/api#2@" + pullToken,
	"acme/api#2:review:77@" + reviewToken,
	"acme/api#3:comment:997@2026-08-15T10:00:00Z",
	"acme/api#1:comment:998@2026-09-01T12:30:00Z",
	"acme/api#2:comment:999@2026-09-03T09:00:00Z",
	"acme/api#2:comment:88@2026-09-04T10:05:00Z",
	"acme/api@" + sha1,
	"acme/api@" + sha2,
}

var liveHooks = []struct{ event, file string }{
	{"issues", "issues.edited"},
	{"issue_comment", "issue_comment.created"},
	{"pull_request", "pull_request.synchronize"},
	{"pull_request_review", "pull_request_review.submitted"},
	{"pull_request_review_comment", "pull_request_review_comment.created"},
	{"push", "push"},
}

var fixtureLists = map[string]string{
	"/repos/acme/api/issues":          "issues.json",
	"/repos/acme/api/pulls":           "pulls.json",
	"/repos/acme/api/pulls/2/reviews": "reviews-2.json",
	"/repos/acme/api/pulls/2/files":   "pulls-2-files.json",
	"/repos/acme/api/issues/comments": "issue-comments.json",
	"/repos/acme/api/pulls/comments":  "review-comments.json",
	"/repos/acme/api/commits":         "commits.json",
}

var fixtureContents = map[string]string{
	"/repos/acme/api/contents":                    "contents-root.json",
	"/repos/acme/api/contents/.github/CODEOWNERS": "contents-codeowners.json",
}

type fakeGitHub struct {
	t       *testing.T
	srv     *httptest.Server
	private atomic.Bool

	mu       sync.Mutex
	lists    map[string][]map[string]any
	contents map[string][]byte
	rewound  map[string]int
	requests []string
	status   map[string]int
	holds    map[string]chan struct{}
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	gh := &fakeGitHub{
		t: t, lists: map[string][]map[string]any{}, contents: map[string][]byte{}, rewound: map[string]int{},
		status: map[string]int{}, holds: map[string]chan struct{}{},
	}
	for path, file := range fixtureLists {
		raw, err := os.ReadFile(filepath.Join("testdata", "api", file))
		if err != nil {
			t.Fatal(err)
		}
		var items []map[string]any
		if err := json.Unmarshal(raw, &items); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		gh.lists[path] = items
	}
	for path, file := range fixtureContents {
		raw, err := os.ReadFile(filepath.Join("testdata", "api", file))
		if err != nil {
			t.Fatal(err)
		}
		gh.contents[path] = raw
	}
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
	path := r.URL.Path
	gh.mu.Lock()
	gh.requests = append(gh.requests, r.URL.RequestURI())
	status, failing := gh.status[path]
	hold := gh.holds[path]
	gh.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if failing {
		http.Error(w, "failing on purpose", status)
		return
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	rest, ok := strings.CutPrefix(path, "/repos/")
	parts := strings.SplitN(rest, "/", 3)
	if !ok || len(parts) < 2 {
		gh.t.Errorf("unexpected request %s", r.URL.RequestURI())
		http.NotFound(w, r)
		return
	}
	repo, sub := parts[0]+"/"+parts[1], ""
	if len(parts) == 3 {
		sub = "/" + parts[2]
	}
	commits := gh.lists["/repos/"+repo+"/commits"]
	head := min(gh.rewound[repo], len(commits))
	q := r.URL.Query()

	switch {
	case sub == "":
		writeJSON(w, map[string]any{"full_name": repo, "private": gh.private.Load(), "default_branch": "main"})
	case sub == "/branches/main":
		if head == len(commits) {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"name": "main", "commit": map[string]any{"sha": commits[head]["sha"]}})
	case strings.HasPrefix(sub, "/compare/"):
		out := slices.Clone(commits[head:])
		slices.Reverse(out)
		writeJSON(w, map[string]any{"total_commits": len(out), "commits": out})
	case sub == "/contents" || strings.HasPrefix(sub, "/contents/"):
		raw, ok := gh.contents[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(raw)
	case sub == "/commits":
		start := head
		if sha := q.Get("sha"); sha != "main" {
			start = slices.IndexFunc(commits, func(c map[string]any) bool { return c["sha"] == sha })
			if start < 0 {
				http.NotFound(w, r)
				return
			}
		}
		gh.paginate(w, r, since(commits[start:], q.Get("since"), "commit", "committer", "date"))
	default:
		items, known := gh.lists[path]
		if !known && !strings.HasSuffix(sub, "/reviews") && !strings.HasSuffix(sub, "/files") && repo != "acme/web" {
			gh.t.Errorf("unexpected request %s", r.URL.RequestURI())
			http.NotFound(w, r)
			return
		}
		if sub != "/pulls" {
			items = since(items, q.Get("since"), "updated_at")
		}
		if sort := q.Get("sort"); sort == "updated" || sort == "created" {
			desc := q.Get("direction") == "desc"
			items = slices.Clone(items)
			slices.SortStableFunc(items, func(a, b map[string]any) int {
				cmp := strings.Compare(field(a, sort+"_at"), field(b, sort+"_at"))
				if desc {
					return -cmp
				}
				return cmp
			})
		}
		gh.paginate(w, r, items)
	}
}

func (gh *fakeGitHub) paginate(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	q := r.URL.Query()
	size, page := 30, 1
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil {
		size = n
	}
	if n, err := strconv.Atoi(q.Get("page")); err == nil {
		page = n
	}
	start := min((page-1)*size, len(items))
	end := min(start+size, len(items))
	if end < len(items) {
		q.Set("page", strconv.Itoa(page+1))
		next := gh.srv.URL + r.URL.Path + "?" + q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="last", <%s>; rel="next"`, next, next))
	}
	out := append([]map[string]any{}, items[start:end]...)
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func field(item map[string]any, keys ...string) string {
	var v any = item
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return s
}

func since(items []map[string]any, s string, keys ...string) []map[string]any {
	if s == "" {
		return items
	}
	var out []map[string]any
	for _, it := range items {
		if field(it, keys...) > s {
			out = append(out, it)
		}
	}
	return out
}

func (gh *fakeGitHub) setList(path string, items []map[string]any) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.lists[path] = items
}

func (gh *fakeGitHub) touch(path string, n int, updatedAt string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for _, it := range gh.lists[path] {
		if fmt.Sprint(it["number"]) == strconv.Itoa(n) {
			it["updated_at"] = updatedAt
			return
		}
	}
	gh.t.Fatalf("no item %d in %s", n, path)
}

func (gh *fakeGitHub) edit(path string, id int, field string, value any) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for _, it := range gh.lists[path] {
		if fmt.Sprint(it["id"]) == strconv.Itoa(id) {
			it[field] = value
			return
		}
	}
	gh.t.Fatalf("no item with id %d in %s", id, path)
}

func (gh *fakeGitHub) rewind(repo string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.rewound[repo]++
}

func (gh *fakeGitHub) setContents(path string, raw []byte) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if raw == nil {
		delete(gh.contents, path)
		return
	}
	gh.contents[path] = raw
}

func (gh *fakeGitHub) fail(path string, status int) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.status[path] = status
}

func (gh *fakeGitHub) hold(path string) (release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	gh.mu.Lock()
	gh.holds[path] = ch
	gh.mu.Unlock()
	gh.t.Cleanup(release)
	return release
}

func (gh *fakeGitHub) seen() []string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return append([]string(nil), gh.requests...)
}

func genItem(n int, created, updated string, pull bool) map[string]any {
	it := map[string]any{
		"number": n, "title": fmt.Sprintf("Item %d", n), "state": "open",
		"html_url":   fmt.Sprintf("https://github.com/acme/api/issues/%d", n),
		"user":       map[string]any{"login": "kpenfound", "node_id": "MDQ6VXNlcjE=", "type": "User"},
		"created_at": created, "updated_at": updated,
	}
	if pull {
		it["head"] = map[string]any{"ref": "topic", "sha": sha1}
		it["base"] = map[string]any{"ref": "main", "sha": sha2}
	}
	return it
}

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

func gateFor(src connector.SourceConfig, c connector.Connector, sink connector.Sink) *connector.Gate {
	return connector.NewGate(sink, src.ID, c.Describe(), connector.NewAllowlist(src))
}

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

func walkStep(t *testing.T, c connector.Backfiller, sink connector.Sink, cursor string, between func()) {
	t.Helper()
	step := stepOf(t, connector.Cursor(cursor))
	cur := connector.Cursor(cursor)
	for i := range 100 {
		res, err := c.Backfill(t.Context(), sink, cur)
		if err != nil {
			t.Fatalf("Backfill(%q) = %v", cur, err)
		}
		if i == 0 && between != nil {
			between()
		}
		if res.Done || stepOf(t, res.Next) != step {
			return
		}
		if res.Next == cur {
			t.Fatalf("Backfill(%q) made no progress", cur)
		}
		cur = res.Next
	}
	t.Fatalf("the walk did not leave step %s in 100 calls", step)
}

func stepOf(t *testing.T, cur connector.Cursor) string {
	t.Helper()
	var pos struct {
		Step string `json:"step"`
	}
	if err := json.Unmarshal([]byte(cur), &pos); err != nil {
		t.Fatalf("cursor %q: %v", cur, err)
	}
	return pos.Step
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

func deliver(t *testing.T, h http.Handler, event string, body []byte) int {
	t.Helper()
	return deliverSigned(t.Context(), t, h, event, body, sign(hookSecret, body))
}

func deliverSigned(ctx context.Context, t *testing.T, h http.Handler, event string, body []byte, signature string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/hooks/"+sourceID, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "d-1")
	req.Header.Set("X-Hub-Signature-256", signature)
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

func artifacts(events []connector.Event) map[string]bool {
	set := map[string]bool{}
	for _, ev := range events {
		set[ev.Payload.Artifact] = true
	}
	return set
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

func backfilledByID(t *testing.T, c *github.Connector, src connector.SourceConfig) map[string]connector.Event {
	t.Helper()
	backfilled := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, backfilled))
	byID := map[string]connector.Event{}
	for _, ev := range backfilled.Events() {
		byID[ev.ID] = ev
	}
	return byID
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

func TestBackfillEmitsEventsInStepOrder(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))
	if got := nativeIDs(rec.Events()); asJSON(t, got) != asJSON(t, wantBackfill) {
		t.Errorf("native ids =\n%v\nwant\n%v", got, wantBackfill)
	}
}

func TestBackfillEventsHaveTheContractsShape(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))

	events := rec.Events()
	byNativeID := map[string]connector.Event{}
	for _, ev := range events {
		byNativeID[ev.NativeID] = ev
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
		token    string
		native   string
		paths    []string
	}{
		{
			nativeID: "acme/api#1@2026-09-02T11:00:00Z", kind: connector.KindIssue, artifact: "acme/api#1", author: kyle,
			title: "/health returns 500", text: "The API returns 500 on /health when the database is up.",
			time: "2026-09-01T10:00:00Z", editedAt: "2026-09-02T11:00:00Z", native: `"labels":["bug"]`,
		},
		{
			nativeID: "acme/api#3@2026-08-20T09:30:00Z", kind: connector.KindIssue, artifact: "acme/api#3", author: robin,
			title: "Document the deploy job", time: "2026-08-01T09:00:00Z", editedAt: "2026-08-20T09:30:00Z",
			native: `"closed_at":"2026-08-20T09:30:00Z"`,
		},
		{
			nativeID: "acme/api#2@" + pullToken, kind: connector.KindPullRequest, artifact: "acme/api#2", author: robin,
			title: "Fix /health", text: "Fixes #1", time: "2026-09-03T08:00:00Z", editedAt: "2026-09-04T12:00:00Z", token: pullToken,
			native: `"head":{"ref":"fix-health"`, paths: pullPaths,
		},
		{
			nativeID: "acme/api#2:review:77@" + reviewToken, kind: connector.KindReview, artifact: "acme/api#2:review:77", parent: "acme/api#2",
			author: kyle, text: "Looks good.", time: "2026-09-04T11:00:00Z", token: reviewToken, native: `"state":"approved"`,
		},
		{
			nativeID: "acme/api#3:comment:997@2026-08-15T10:00:00Z", kind: connector.KindMessage, artifact: "acme/api#3:comment:997", parent: "acme/api#3",
			author: kyle, text: "Done in the runbook.", time: "2026-08-15T10:00:00Z", editedAt: "2026-08-15T10:00:00Z",
		},
		{
			nativeID: "acme/api#1:comment:998@2026-09-01T12:30:00Z", kind: connector.KindMessage, artifact: "acme/api#1:comment:998", parent: "acme/api#1",
			author: robin, text: "Seeing this too.", time: "2026-09-01T12:00:00Z", editedAt: "2026-09-01T12:30:00Z",
		},
		{
			nativeID: "acme/api#2:comment:999@2026-09-03T09:00:00Z", kind: connector.KindMessage, artifact: "acme/api#2:comment:999", parent: "acme/api#2",
			author: shed, text: "CI passed.", time: "2026-09-03T09:00:00Z", editedAt: "2026-09-03T09:00:00Z",
		},
		{
			nativeID: "acme/api#2:comment:88@2026-09-04T10:05:00Z", kind: connector.KindReviewComment, artifact: "acme/api#2:comment:88", parent: "acme/api#2",
			author: kyle, text: "Nit: name this handler.", time: "2026-09-04T10:00:00Z", editedAt: "2026-09-04T10:05:00Z",
			native: `"path":"api/health.go"`,
		},
		{
			nativeID: "acme/api@" + sha1, kind: connector.KindCommit, artifact: "acme/api@" + sha1, author: committer,
			title: "Fix /health (#2)", text: "Fix /health (#2)\n\nReturn 200 when the database is up.",
			time: "2026-09-04T13:05:00Z", native: `"parents":["` + sha2 + `"]`,
		},
		{
			nativeID: "acme/api@" + sha2, kind: connector.KindCommit, artifact: "acme/api@" + sha2, author: ghost,
			title: "Initial commit", text: "Initial commit", time: "2026-08-01T08:00:00Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.nativeID, func(t *testing.T) {
			ev, ok := byNativeID[tt.nativeID]
			if !ok {
				t.Fatalf("no event %s", tt.nativeID)
			}
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
			case tt.token != "":
				var edited string
				if p.Revision != nil && !p.Revision.EditedAt.IsZero() {
					edited = p.Revision.EditedAt.Format(time.RFC3339)
				}
				if p.Revision == nil || p.Revision.Token != tt.token || edited != tt.editedAt {
					t.Errorf("revision = %+v, want token %s and edited_at %q", p.Revision, tt.token, tt.editedAt)
				}
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
			if !slices.Equal(p.Paths, tt.paths) || p.PathsTruncated {
				t.Errorf("paths = %v, truncated %v; want %v, not truncated", p.Paths, p.PathsTruncated, tt.paths)
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

func TestBackfillAtASmallPageSizeEmitsEverything(t *testing.T) {
	t.Cleanup(github.SetPageSize(1))
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))

	got := slices.Compact(slices.Sorted(slices.Values(nativeIDs(rec.Events()))))
	want := slices.Sorted(slices.Values(wantBackfill))
	if !slices.Equal(got, want) {
		t.Errorf("native ids =\n%v\nwant\n%v", got, want)
	}
}

func TestBackfillEmitsWhatDidNotChangeWhileSomethingElseDid(t *testing.T) {
	t.Cleanup(github.SetPageSize(2))
	tests := []struct {
		name, step, path string
		pull             bool
	}{
		{name: "issues, walked by updated_at", step: "issues", path: "/repos/acme/api/issues"},
		{name: "pull requests, walked in creation order", step: "pulls", path: "/repos/acme/api/pulls", pull: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			var items []map[string]any
			for i := range 5 {
				at := time.Date(2026, time.September, 1, i, 0, 0, 0, time.UTC).Format(time.RFC3339)
				items = append(items, genItem(10+i, at, at, tt.pull))
			}
			gh.setList(tt.path, items)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			rec := &connector.Recorder{}

			walkStep(t, c, gateFor(src, c, rec), `{"repo":"acme/api","step":"`+tt.step+`"}`, func() {
				gh.touch(tt.path, 10, "2026-09-10T00:00:00Z")
			})
			seen := artifacts(rec.Events())
			for n := 10; n < 15; n++ {
				if a := "acme/api#" + strconv.Itoa(n); !seen[a] {
					t.Errorf("%s was never emitted; emitted %v", a, nativeIDs(rec.Events()))
				}
			}
		})
	}
}

func TestBackfillPagesThroughTiesWiderThanAPage(t *testing.T) {
	t.Cleanup(github.SetPageSize(2))
	gh := newFakeGitHub(t)
	at := "2026-09-01T00:00:00Z"
	gh.setList("/repos/acme/api/issues", []map[string]any{genItem(10, at, at, false), genItem(11, at, at, false), genItem(12, at, at, false)})
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}

	walkStep(t, c, gateFor(src, c, rec), `{"repo":"acme/api","step":"issues"}`, nil)
	seen := artifacts(rec.Events())
	for _, a := range []string{"acme/api#10", "acme/api#11", "acme/api#12"} {
		if !seen[a] {
			t.Errorf("%s was never emitted; emitted %v", a, nativeIDs(rec.Events()))
		}
	}
}

func TestBackfillOfCommitsStaysOnTheHistoryItStarted(t *testing.T) {
	t.Cleanup(github.SetPageSize(1))
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}

	walkStep(t, c, gateFor(src, c, rec), `{"repo":"acme/api","step":"commits"}`, func() {
		gh.rewind("acme/api")
	})
	if got := nativeIDs(rec.Events()); !slices.Equal(got, []string{"acme/api@" + sha1, "acme/api@" + sha2}) {
		t.Errorf("native ids = %v, want both commits", got)
	}
}

func TestBackfillResumesFromItsCursorInANewConnector(t *testing.T) {
	t.Cleanup(github.SetPageSize(1))
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	whole := &connector.Recorder{}
	c := newConnector(t, src)
	backfillAll(t, c, gateFor(src, c, whole))

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
}

func TestBackfillCursorsAreStorableAndHoldNoHost(t *testing.T) {
	t.Cleanup(github.SetPageSize(1))
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	cursors := backfillAll(t, c, gateFor(src, c, &connector.Recorder{}))
	for _, cur := range cursors {
		if len(cur) > connector.MaxCursorLen || !utf8.ValidString(string(cur)) || strings.ContainsRune(string(cur), 0) {
			t.Errorf("cursor %q is not one the runtime can store", cur)
		}
		if strings.Contains(string(cur), gh.srv.URL) || strings.Contains(string(cur), "127.0.0.1") {
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
			wantEvents: 2,
			wantNext:   `{"repo":"acme/api","step":"pulls"}`,
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
		{
			name:       "a keyset position reads from its updated_at",
			cursor:     `{"repo":"acme/api","step":"issues","since":"2026-09-02T10:59:59Z"}`,
			wantEvents: 1,
			wantNext:   `{"repo":"acme/api","step":"pulls"}`,
		},
		{
			name:       "a pinned commit position reads that commit's history",
			cursor:     `{"repo":"acme/api","step":"commits","head":"` + sha2 + `"}`,
			wantDone:   true,
			wantEvents: 1,
		},
		{name: "a cursor that is not JSON", cursor: "3", wantErr: "not one the github connector wrote"},
		{name: "a step it does not have", cursor: `{"repo":"acme/api","step":"labels"}`, wantErr: `step "labels"`},
		{name: "a since that is not a timestamp", cursor: `{"repo":"acme/api","step":"issues","since":"yesterday"}`, wantErr: `since "yesterday"`},
		{name: "a since that is not a timestamp on the commits step", cursor: `{"repo":"acme/api","step":"commits","since":"yesterday"}`, wantErr: `since "yesterday"`},
		{name: "a negative page", cursor: `{"repo":"acme/api","step":"pulls","page":-1}`, wantErr: "page -1"},
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
	if last := seen[len(seen)-1]; last != "/repos/acme/web/branches/main" {
		t.Errorf("last request = %s, want acme/web's branch after acme/api's walk", last)
	}
}

func TestBackfillSinceSkipsWhatPredatesTheStartDate(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, map[string]any{"since": "2026-09-01"})
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))

	var want []string
	for _, id := range wantBackfill {
		if !strings.HasPrefix(id, "acme/api#3") && id != "acme/api@"+sha2 {
			want = append(want, id)
		}
	}
	if got := nativeIDs(rec.Events()); asJSON(t, got) != asJSON(t, want) {
		t.Errorf("native ids = %v, want %v", got, want)
	}
}

func TestBackfillSinceIsSentAsASecondBeforeTheStartDate(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, map[string]any{"since": "2026-09-01"})
	c := newConnector(t, src)
	backfillAll(t, c, gateFor(src, c, &connector.Recorder{}))
	for _, uri := range gh.seen() {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		since := u.Query().Get("since")
		switch {
		case strings.HasSuffix(u.Path, "/issues"), strings.HasSuffix(u.Path, "/comments"), strings.HasSuffix(u.Path, "/commits"):
			if since != "2026-08-31T23:59:59Z" {
				t.Errorf("%s: since = %q, want a second before the configured start date", uri, since)
			}
		case strings.HasSuffix(u.Path, "/pulls"):
			if since != "" {
				t.Errorf("%s: since = %q, want none", uri, since)
			}
		}
	}
}

func TestBackfillSinceIncludesTheStartDate(t *testing.T) {
	gh := newFakeGitHub(t)
	before, at := "2026-08-31T23:59:59Z", "2026-09-01T00:00:00Z"
	gh.setList("/repos/acme/api/issues", []map[string]any{genItem(10, before, before, false), genItem(11, at, at, false)})
	gh.setList("/repos/acme/api/pulls", []map[string]any{genItem(20, before, before, true), genItem(21, at, at, true)})
	src := newSource(t, gh, sourceID, map[string]any{"since": "2026-09-01"})
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))

	seen := artifacts(rec.Events())
	for artifact, want := range map[string]bool{"acme/api#10": false, "acme/api#11": true, "acme/api#20": false, "acme/api#21": true} {
		if seen[artifact] != want {
			t.Errorf("%s emitted = %v, want %v; emitted %v", artifact, seen[artifact], want, nativeIDs(rec.Events()))
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
			name:    "a list page fails",
			setup:   func(gh *fakeGitHub) { gh.fail("/repos/acme/api/issues", http.StatusBadGateway) },
			wantErr: "502",
		},
		{
			name:    "a review page fails after its pull request was emitted",
			cursor:  `{"repo":"acme/api","step":"pulls"}`,
			setup:   func(gh *fakeGitHub) { gh.fail("/repos/acme/api/pulls/2/reviews", http.StatusInternalServerError) },
			wantErr: "500",
		},
		{
			name:    "the files of a pull request cannot be read",
			cursor:  `{"repo":"acme/api","step":"pulls"}`,
			setup:   func(gh *fakeGitHub) { gh.fail("/repos/acme/api/pulls/2/files", http.StatusBadGateway) },
			wantErr: "502",
		},
		{
			name:    "the default branch cannot be read",
			cursor:  `{"repo":"acme/api","step":"commits"}`,
			setup:   func(gh *fakeGitHub) { gh.fail("/repos/acme/api/branches/main", http.StatusInternalServerError) },
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

func TestBackfillOfAnEmptyRepositoryFinishes(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.fail("/repos/acme/api/branches/main", http.StatusNotFound)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	res, err := c.Backfill(t.Context(), gateFor(src, c, &connector.Recorder{}), `{"repo":"acme/api","step":"commits"}`)
	if err != nil || !res.Done || res.Events != 0 {
		t.Errorf("Backfill = %+v, %v; want done with nothing", res, err)
	}
}

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

func TestWebhookLogsCarryIdsAndNoPayloadOrCredentials(t *testing.T) {
	const secretText = "CONFIDENTIAL-COMMENT-TEXT"
	repo := `"repository":{"full_name":"acme/api","private":false,"default_branch":"main"}`
	forged := "sha256=" + strings.Repeat("ab", 32)
	malformed := []byte(`{"action":"created","comment":{"id":1,"body":"` + secretText + `","issue_url":"https://api.github.com/repos/acme/api/issues/"},` + repo + `}`)
	tests := []struct {
		name, event string
		body        []byte
		signature   string
		sinkErr     error
		wantLog     string
	}{
		{name: "a refused signature", event: "issues", body: hook(t, "issues.edited"), signature: forged, wantLog: "signature does not verify"},
		{name: "a delivery it cannot read", event: "issue_comment", body: malformed, wantLog: "webhook delivery refused"},
		{name: "a store failure", event: "issues", body: hook(t, "issues.edited"), sinkErr: errors.New("store is down"), wantLog: "webhook delivery failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			var logs bytes.Buffer
			ctx := telemetry.WithLogger(t.Context(), slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			signature := tt.signature
			if signature == "" {
				signature = sign(hookSecret, tt.body)
			}
			h := c.Handler(gateFor(src, c, &connector.Recorder{Err: tt.sinkErr}))
			deliverSigned(ctx, t, h, tt.event, tt.body, signature)
			out := logs.String()
			if !strings.Contains(out, tt.wantLog) || !strings.Contains(out, "d-1") {
				t.Fatalf("log = %q, want %q naming delivery d-1", out, tt.wantLog)
			}
			for _, leaked := range []string{secretText, "The API returns 500", "/health returns 500", testToken, hookSecret, forged, sign(hookSecret, tt.body)} {
				if strings.Contains(out, leaked) {
					t.Errorf("log output holds %q: %s", leaked, out)
				}
			}
		})
	}
}

func TestWebhookEventsEqualBackfilledEvents(t *testing.T) {
	for _, private := range []bool{false, true} {
		t.Run(fmt.Sprintf("private=%v", private), func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.private.Store(private)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			byID := backfilledByID(t, c, src)

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

func TestPushReadsItsCommitsInOneCall(t *testing.T) {
	tests := []struct {
		name, before, wantPath string
	}{
		{name: "a push that moved the branch compares it", before: baseSHA, wantPath: "/repos/acme/api/compare/" + baseSHA + "..." + sha1},
		{name: "the push that created the branch reads its history", before: nullSHA, wantPath: "/repos/acme/api/commits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			byID := backfilledByID(t, c, src)

			earlier := len(gh.seen())
			body := bytes.Replace(hook(t, "push"), []byte(`"before": "`+baseSHA+`"`), []byte(`"before": "`+tt.before+`"`), 1)
			live := &connector.Recorder{}
			if code := deliver(t, c.Handler(gateFor(src, c, live)), "push", body); code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", code)
			}
			requests := gh.seen()[earlier:]
			if len(requests) != 1 || !strings.HasPrefix(requests[0], tt.wantPath) {
				t.Errorf("requests = %v, want one to %s", requests, tt.wantPath)
			}
			events := live.Events()
			if len(events) != 2 {
				t.Fatalf("push emitted %v, want both commits", nativeIDs(events))
			}
			for _, ev := range events {
				if a, b := asJSON(t, ev), asJSON(t, byID[ev.ID]); a != b {
					t.Errorf("pushed commit differs from the backfilled one:\n%s\nwant\n%s", a, b)
				}
			}
		})
	}
}

func TestPushThatCannotReadInTimeFails(t *testing.T) {
	t.Cleanup(github.SetReadTimeout(50 * time.Millisecond))
	gh := newFakeGitHub(t)
	release := gh.hold("/repos/acme/api/compare/" + baseSHA + "..." + sha1)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}

	start := time.Now()
	code := deliver(t, c.Handler(gateFor(src, c, rec)), "push", hook(t, "push"))
	elapsed := time.Since(start)
	release()
	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", code)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the delivery took %v, want it bounded by the read timeout", elapsed)
	}
	if n := len(rec.Events()); n != 0 {
		t.Errorf("%d events emitted, want none", n)
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

func TestAReviewEditedOrDismissedIsANewRevision(t *testing.T) {
	const reviews = "/repos/acme/api/pulls/2/reviews"
	submitted := string(hook(t, "pull_request_review.submitted"))
	tests := []struct {
		name      string
		action    string
		field     string
		rest      string
		hookValue string
		token     string
	}{
		{
			name: "an edited body", action: "edited", field: "body",
			rest: "Looks good, once the handler is named.", hookValue: `"body": "Looks good, once the handler is named."`,
			token: "56cc310aec1b377c",
		},
		{
			name: "a dismissal", action: "dismissed", field: "state",
			rest: "DISMISSED", hookValue: `"state": "dismissed"`,
			token: "8e96a94ea376225d",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			c := newConnector(t, src)
			before := &connector.Recorder{}
			backfillAll(t, c, gateFor(src, c, before))

			old := map[string]string{"body": `"body": "Looks good."`, "state": `"state": "approved"`}[tt.field]
			body := strings.Replace(strings.Replace(submitted, `"action": "submitted"`, `"action": "`+tt.action+`"`, 1), old, tt.hookValue, 1)
			live := &connector.Recorder{}
			if code := deliver(t, c.Handler(gateFor(src, c, live)), "pull_request_review", []byte(body)); code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", code)
			}
			hooked := live.Events()
			if len(hooked) != 1 {
				t.Fatalf("webhook emitted %v, want one review", nativeIDs(hooked))
			}

			gh.edit(reviews, 77, tt.field, tt.rest)
			after := &connector.Recorder{}
			again := newConnector(t, src)
			backfillAll(t, again, gateFor(src, again, after))

			var original, backfilled connector.Event
			for _, ev := range before.Events() {
				if ev.Kind == connector.KindReview {
					original = ev
				}
			}
			for _, ev := range after.Events() {
				if ev.Kind == connector.KindReview {
					backfilled = ev
				}
			}
			want := "acme/api#2:review:77@" + tt.token
			if backfilled.NativeID != want || backfilled.Payload.Revision == nil || backfilled.Payload.Revision.Token != tt.token {
				t.Errorf("backfilled review = %s with revision %+v, want %s", backfilled.NativeID, backfilled.Payload.Revision, want)
			}
			if backfilled.ID == original.ID || backfilled.Payload.Artifact != original.Payload.Artifact {
				t.Errorf("backfilled review is %s of %s, want a new id for artifact %s", backfilled.NativeID, backfilled.Payload.Artifact, original.Payload.Artifact)
			}
			if !backfilled.Time.Equal(original.Time) {
				t.Errorf("time = %v, want the submission's %v", backfilled.Time, original.Time)
			}
			if a, b := asJSON(t, hooked[0]), asJSON(t, backfilled); a != b {
				t.Errorf("webhook event differs from the backfilled one:\n%s\nwant\n%s", a, b)
			}
		})
	}
}

func TestWebhookIgnoresWhatItDoesNotIngest(t *testing.T) {
	repo := `"repository":{"full_name":"acme/api","private":false,"default_branch":"main"}`
	pending := strings.Replace(string(hook(t, "pull_request_review.submitted")), `"state": "approved"`, `"state": "pending"`, 1)
	pendingEdited := strings.Replace(pending, `"action": "submitted"`, `"action": "edited"`, 1)
	requested := strings.Replace(string(hook(t, "pull_request_review.submitted")), `"action": "submitted"`, `"action": "review_requested"`, 1)
	tests := []struct {
		name, event, body string
	}{
		{"a ping", "ping", `{"zen":"Keep it logically awesome.","hook_id":1}`},
		{"a push to another branch", "push", `{"ref":"refs/heads/feature","before":"` + baseSHA + `","after":"` + sha1 + `",` + repo + `}`},
		{"a push that deletes the default branch", "push", `{"ref":"refs/heads/main","before":"` + sha1 + `","after":"` + nullSHA + `",` + repo + `}`},
		{"a tag push", "push", `{"ref":"refs/tags/main","before":"` + baseSHA + `","after":"` + sha1 + `",` + repo + `}`},
		{"a pending review", "pull_request_review", pending},
		{"an edit to a pending review", "pull_request_review", pendingEdited},
		{"a review action it does not read", "pull_request_review", requested},
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
		{"a sub_issues event with no sub-issue", "sub_issues", `{"action":"parent_issue_added","parent_issue":{"url":"https://api.github.com/repos/acme/api/issues/10","number":10},` + repo + `}`},
		{"a sub-issue whose parent url names no issue", "sub_issues", `{"action":"parent_issue_added","parent_issue":{"url":"https://api.github.com/repos/acme/api","number":10},"sub_issue":{"url":"https://api.github.com/repos/acme/api/issues/12","repository_url":"https://api.github.com/repos/acme/api","number":12,"title":"x","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"},` + repo + `}`},
		{"a comment with no issue url", "issue_comment", `{"action":"created","comment":{"id":1,"body":"x","issue_url":"https://api.github.com/repos/acme/api/issues/"},` + repo + `}`},
		{"a deleted review comment with no pull request url", "pull_request_review_comment", `{"action":"deleted","comment":{"id":1},` + repo + `}`},
		{"a review with no pull request", "pull_request_review", `{"action":"submitted","review":{"id":1,"state":"approved","submitted_at":"2026-09-01T00:00:00Z"},` + repo + `}`},
		{"a push whose after is not a commit id", "push", `{"ref":"refs/heads/main","before":"` + baseSHA + `","after":"../../../user",` + repo + `}`},
		{"a push whose after is too short to be a commit id", "push", `{"ref":"refs/heads/main","before":"` + baseSHA + `","after":"c0ffee",` + repo + `}`},
		{"a push with no before", "push", `{"ref":"refs/heads/main","after":"` + sha1 + `",` + repo + `}`},
		{"a push with an upper-case commit id", "push", `{"ref":"refs/heads/main","before":"` + strings.ToUpper(sha2) + `","after":"` + sha1 + `",` + repo + `}`},
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
			name: "a re-sync cannot be recorded", event: "repository", file: "repository.privatized",
			setup: func(*fakeGitHub, *connector.Recorder) {},
		},
		{
			name: "the files of a pull request cannot be read", event: "pull_request", file: "pull_request.synchronize",
			setup: func(gh *fakeGitHub, _ *connector.Recorder) {
				gh.fail("/repos/acme/api/pulls/2/files", http.StatusInternalServerError)
			},
		},
		{
			name: "a pushed commit cannot be read", event: "push", file: "push",
			setup: func(gh *fakeGitHub, _ *connector.Recorder) {
				gh.fail("/repos/acme/api/compare/"+baseSHA+"..."+sha1, http.StatusInternalServerError)
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

func TestWebhookForARepositoryTheSourceDoesNotListIsDropped(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	body := bytes.ReplaceAll(hook(t, "issues.edited"), []byte(`"full_name": "acme/api"`), []byte(`"full_name": "acme/other"`))
	gate := gateFor(src, c, rec)
	deliver(t, c.Handler(gate), "issues", body)
	if n := len(rec.Events()); n != 0 || gate.Dropped() != 1 {
		t.Errorf("%d events emitted and %d dropped for an unlisted repository, want none emitted and one dropped", n, gate.Dropped())
	}
}

func TestWebhookUsesTheConfiguredSpellingOfTheRepository(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	body := bytes.ReplaceAll(hook(t, "issues.edited"), []byte(`"full_name": "acme/api"`), []byte(`"full_name": "Acme/API"`))
	if code := deliver(t, c.Handler(gateFor(src, c, rec)), "issues", body); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	want := "acme/api#1@2026-09-02T11:00:00Z"
	events := rec.Events()
	if len(events) != 1 || events[0].NativeID != want || events[0].Payload.Container.NativeID != "acme/api" {
		t.Errorf("events = %s, want %s in acme/api", asJSON(t, events), want)
	}
}

func runtimeFor(t *testing.T, src connector.SourceConfig, opts connector.RuntimeOptions) (*connector.Runtime, func()) {
	t.Helper()
	reg := connector.NewRegistry()
	if err := reg.Register(github.Type, github.Factory); err != nil {
		t.Fatal(err)
	}
	opts.Sources = []connector.SourceConfig{src}
	opts.Registry = reg
	opts.Lookup = func(v string) (string, bool) { return v, true }
	opts.Cadence = connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}
	rt, err := connector.NewRuntime(t.Context(), opts)
	if err != nil {
		t.Fatalf("NewRuntime = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("the runtime did not stop within 10s")
			}
		})
	}
	t.Cleanup(stop)
	return rt, stop
}

func requestedSince(gh *fakeGitHub, mark int, path string) func() bool {
	return func() bool {
		return slices.ContainsFunc(gh.seen()[mark:], func(uri string) bool { return strings.HasPrefix(uri, path+"?") })
	}
}

func resynced(t *testing.T, store connector.ResyncStore, source, container string) func() bool {
	return func() bool {
		records, err := store.Resyncs(t.Context(), source)
		if err != nil {
			t.Fatalf("reading the re-syncs: %v", err)
		}
		for _, r := range records {
			if r.Container == container {
				return r.Generation > 0 && !r.Owed
			}
		}
		return false
	}
}

func assertEveryArtifactPrivate(t *testing.T, public []connector.Event, l0 *connector.Recorder) {
	t.Helper()
	current := map[string]connector.Event{}
	for _, ev := range l0.Events() {
		current[ev.Payload.Artifact] = ev
	}
	for artifact := range artifacts(public) {
		ev, ok := current[artifact]
		if !ok || !strings.HasSuffix(ev.NativeID, "perm:private") {
			t.Errorf("current revision of %s is %q, want one under perm:private", artifact, ev.NativeID)
		}
	}
	exposed, err := l0.Exposed(t.Context(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exposed) != 0 {
		t.Errorf("L0 still serves %+v as public", exposed)
	}
}

type privatizedRun struct {
	public  []connector.Event
	emitted []connector.Event
	later   []string
}

func runPrivatizedResync(t *testing.T) privatizedRun {
	t.Helper()
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil, "acme/api", "acme/web")
	c := newConnector(t, src)
	public := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, public))

	gh.private.Store(true)
	resyncs := connector.NewMemoryResyncs()
	rec := &connector.Recorder{}
	rt, stop := runtimeFor(t, src, connector.RuntimeOptions{Sink: rec, Resyncs: resyncs})
	mark := len(gh.seen())
	if code := deliver(t, rt.Handler(), "repository", hook(t, "repository.privatized")); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	waitFor(t, "the re-sync", resynced(t, resyncs, sourceID, "acme/api"))
	stop()
	return privatizedRun{public: public.Events(), emitted: rec.Events(), later: gh.seen()[mark:]}
}

func TestBackfillHealthReportsLastEvent(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	backfillAll(t, c, gateFor(src, c, &connector.Recorder{}))
	if h := c.Health(t.Context()); h.Status != connector.HealthOK || h.LastEventAt.IsZero() {
		t.Errorf("health = %+v, want ok with a last event", h)
	}
}

func TestRepositoryGoingPrivateResyncsEveryArtifact(t *testing.T) {
	run := runPrivatizedResync(t)
	assertPrivateOf(t, run.public, run.emitted)
}

func TestResyncReadsOnlyTheRepositoryThatWentPrivate(t *testing.T) {
	for _, uri := range runPrivatizedResync(t).later {
		if strings.HasPrefix(uri, "/repos/acme/web") {
			t.Errorf("the re-sync of acme/api read %s", uri)
		}
	}
}

func TestResyncReachesACommitAPushEmittedBeforeTheStartDate(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, map[string]any{"since": "2026-09-01"})
	rec := &connector.Recorder{}
	resyncs := connector.NewMemoryResyncs()
	rt, stop := runtimeFor(t, src, connector.RuntimeOptions{Sink: rec, Resyncs: resyncs})

	if code := deliver(t, rt.Handler(), "push", hook(t, "push")); code != http.StatusAccepted {
		t.Fatalf("push: status = %d, want 202", code)
	}
	old := "acme/api@" + sha2
	if !slices.Contains(nativeIDs(rec.Events()), old) {
		t.Fatalf("the push emitted %v, want %s", nativeIDs(rec.Events()), old)
	}

	gh.private.Store(true)
	if code := deliver(t, rt.Handler(), "repository", hook(t, "repository.privatized")); code != http.StatusAccepted {
		t.Fatalf("privatized: status = %d, want 202", code)
	}
	waitFor(t, "the re-sync", resynced(t, resyncs, sourceID, "acme/api"))
	stop()
	if want := old + "@perm:private"; !slices.Contains(nativeIDs(rec.Events()), want) {
		t.Errorf("the re-sync emitted %v, want %s", nativeIDs(rec.Events()), want)
	}
}

func TestAResyncInterruptedByARestartResumes(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	l0 := &connector.Recorder{}
	c := newConnector(t, src)
	backfillAll(t, c, gateFor(src, c, l0))
	public := l0.Events()

	gh.private.Store(true)
	resyncs := connector.NewMemoryResyncs()
	commits := "/repos/acme/api/commits"
	release := gh.hold(commits)
	first, stopFirst := runtimeFor(t, src, connector.RuntimeOptions{Sink: l0, Resyncs: resyncs})
	begun := len(gh.seen())
	if code := deliver(t, first.Handler(), "repository", hook(t, "repository.privatized")); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	waitFor(t, "the re-sync to reach the commits", requestedSince(gh, begun, commits))
	stopFirst()
	release()

	rec := resyncs.Get(sourceID, "acme/api")
	if !rec.Owed || rec.Cursor == "" || stepOf(t, rec.Cursor) != "commits" {
		t.Fatalf("after the restart the store holds %+v, want an owed re-sync at the commits", rec)
	}

	mark := len(gh.seen())
	_, stop := runtimeFor(t, src, connector.RuntimeOptions{Sink: l0, Resyncs: resyncs})
	waitFor(t, "the resumed re-sync", resynced(t, resyncs, sourceID, "acme/api"))
	stop()
	for _, uri := range gh.seen()[mark:] {
		if strings.HasPrefix(uri, "/repos/acme/api/issues") || strings.HasPrefix(uri, "/repos/acme/api/pulls") {
			t.Errorf("the restarted re-sync read %s: it started again rather than resuming", uri)
		}
	}
	assertEveryArtifactPrivate(t, public, l0)
}

func TestAResyncWhoseDeliveryWasNeverHandledRunsAtStartup(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	l0 := &connector.Recorder{}
	c := newConnector(t, src)
	backfillAll(t, c, gateFor(src, c, l0))
	public := l0.Events()

	gh.private.Store(true)
	down, stopDown := runtimeFor(t, src, connector.RuntimeOptions{Sink: l0})
	if code := deliver(t, down.Handler(), "repository", hook(t, "repository.privatized")); code != http.StatusInternalServerError {
		t.Fatalf("a delivery with nowhere to record the re-sync: status = %d, want 500", code)
	}
	stopDown()

	resyncs := connector.NewMemoryResyncs()
	_, stop := runtimeFor(t, src, connector.RuntimeOptions{Sink: l0, Resyncs: resyncs, Exposure: l0})
	waitFor(t, "the re-sync nobody asked for", resynced(t, resyncs, sourceID, "acme/api"))
	stop()
	assertEveryArtifactPrivate(t, public, l0)

	mark := len(gh.seen())
	emitted := len(l0.Events())
	checked := &exposureSignal{ExposureReader: l0, read: make(chan struct{}, 1)}
	_, stopAgain := runtimeFor(t, src, connector.RuntimeOptions{Sink: l0, Resyncs: resyncs, Exposure: checked})
	select {
	case <-checked.read:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime did not run its startup check")
	}
	stopAgain()
	if seen := gh.seen()[mark:]; len(seen) != 0 {
		t.Errorf("a runtime started after the re-sync read %v, want nothing", seen)
	}
	if n := len(l0.Events()); n != emitted {
		t.Errorf("a runtime started after the re-sync emitted %d events, want none", n-emitted)
	}
}

type exposureSignal struct {
	connector.ExposureReader
	read chan struct{}
}

func (e *exposureSignal) Exposed(ctx context.Context, source string) ([]connector.Exposure, error) {
	exposed, err := e.ExposureReader.Exposed(ctx, source)
	select {
	case e.read <- struct{}{}:
	default:
	}
	return exposed, err
}

func TestPublic(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	for _, private := range []bool{false, true} {
		gh.private.Store(private)
		public, err := c.Public(t.Context(), "acme/api")
		if err != nil || public == private {
			t.Errorf("Public(acme/api) with private=%v = %v, %v", private, public, err)
		}
	}
	if _, err := c.Public(t.Context(), "acme/other"); err == nil {
		t.Error("Public(acme/other) = no error, want one: the source does not name it")
	}
}

func TestResyncRefusesAForeignCursor(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil, "acme/api", "acme/web")
	c := newConnector(t, src)
	tests := []struct {
		name, container string
		cursor          connector.Cursor
	}{
		{"a repository the source does not name", "acme/other", ""},
		{"another repository's cursor", "acme/api", `{"repo":"acme/web","step":"issues"}`},
		{"a step the connector does not have", "acme/api", `{"repo":"acme/api","step":"wikis"}`},
		{"not a cursor", "acme/api", `nope`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.Resync(t.Context(), &connector.Recorder{}, tt.container, tt.cursor); err == nil {
				t.Errorf("Resync(%s, %s) = no error, want one", tt.container, tt.cursor)
			}
		})
	}
}

func hierarchyList(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "api", "issues-hierarchy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	return items
}

type subIssue struct{ nativeID, partOf string }

var subIssueBackfill = []subIssue{
	{"acme/api#10@2026-09-10T08:00:00Z", ""},
	{"acme/api#11@2026-09-10T09:00:00Z+parent:acme/api#10", "acme/api#10"},
	{"acme/api#12@2026-09-10T10:00:00Z", ""},
	{"acme/api#14@2026-09-10T11:00:00Z+parent:acme/web#7", "acme/web#7"},
}

func backfilledHierarchy(t *testing.T) (*github.Connector, connector.SourceConfig, *connector.Recorder) {
	t.Helper()
	gh := newFakeGitHub(t)
	gh.setList("/repos/acme/api/issues", hierarchyList(t))
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	backfilled := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, backfilled))
	return c, src, backfilled
}

func TestABackfilledSubIssueIsPartOfItsParent(t *testing.T) {
	_, _, backfilled := backfilledHierarchy(t)
	var got []subIssue
	for _, ev := range backfilled.Events() {
		if ev.Kind == connector.KindIssue {
			got = append(got, subIssue{ev.NativeID, ev.Payload.PartOf})
		}
	}
	if !slices.Equal(got, subIssueBackfill) {
		t.Errorf("backfilled issues = %v, want %v", got, subIssueBackfill)
	}
}

func TestSubIssueDeliveries(t *testing.T) {
	c, src, _ := backfilledHierarchy(t)
	live := &connector.Recorder{}
	h := c.Handler(gateFor(src, c, live))
	tests := []struct {
		name, event, file string
		edit              func([]byte) []byte
		want              *subIssue
	}{
		{
			name: "an edit to a sub-issue", event: "issues", file: "issues.edited.sub_issue",
			want: &subIssueBackfill[1],
		},
		{
			name: "a parent added", event: "sub_issues", file: "sub_issues.parent_issue_added",
			want: &subIssue{"acme/api#12@2026-09-12T09:00:00Z+parent:acme/api#10", "acme/api#10"},
		},
		{
			name: "the same relationship delivered to the parent's repository", event: "sub_issues", file: "sub_issues.sub_issue_added",
			want: &subIssue{"acme/api#12@2026-09-12T09:00:00Z+parent:acme/api#10", "acme/api#10"},
		},
		{
			name: "moved to another parent", event: "sub_issues", file: "sub_issues.parent_issue_added.reparent",
			want: &subIssue{"acme/api#11@2026-09-13T10:00:00Z+parent:acme/api#12", "acme/api#12"},
		},
		{
			name: "the old parent's removal, after the move, keeps the new parent", event: "sub_issues", file: "sub_issues.parent_issue_removed.reparent",
			want: &subIssue{"acme/api#11@2026-09-13T10:00:00Z+parent:acme/api#12", "acme/api#12"},
		},
		{
			name: "a parent removed", event: "sub_issues", file: "sub_issues.parent_issue_removed",
			want: &subIssue{"acme/api#12@2026-09-14T08:00:00Z", ""},
		},
		{
			name: "a removal whose sub-issue still names the parent removed", event: "sub_issues", file: "sub_issues.parent_issue_removed",
			edit: func(body []byte) []byte {
				return bytes.Replace(body, []byte(`"number": 12,`), []byte(`"number": 12, "parent_issue_url": "https://api.github.com/repos/acme/api/issues/10",`), 1)
			},
			want: &subIssue{"acme/api#12@2026-09-14T08:00:00Z", ""},
		},
		{
			name: "a sub-issue in another repository is that repository's delivery", event: "sub_issues", file: "sub_issues.sub_issue_added.other_repository",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(live.Events())
			body := hook(t, tt.file)
			if tt.edit != nil {
				body = tt.edit(body)
			}
			code := deliver(t, h, tt.event, body)
			events := live.Events()[before:]
			if tt.want == nil {
				if code != http.StatusNoContent || len(events) != 0 {
					t.Errorf("status = %d with events %v, want 204 and none", code, nativeIDs(events))
				}
				return
			}
			if code != http.StatusAccepted || len(events) != 1 {
				t.Fatalf("status = %d with events %v, want 202 and one", code, nativeIDs(events))
			}
			ev := events[0]
			if g := (subIssue{ev.NativeID, ev.Payload.PartOf}); g != *tt.want {
				t.Errorf("event = %v, want %v", g, *tt.want)
			}
			token := strings.TrimPrefix(ev.NativeID, ev.Payload.Artifact+"@")
			if ev.Payload.Revision == nil || ev.Payload.Revision.Token != token {
				t.Errorf("revision = %+v, want token %s", ev.Payload.Revision, token)
			}
			if ev.Payload.Parent != "" || ev.Payload.Thread != "" {
				t.Errorf("parent, thread = %q, %q; want neither: a parent issue is not a conversation", ev.Payload.Parent, ev.Payload.Thread)
			}
		})
	}
}

func TestASubIssueEditEqualsTheBackfilledEvent(t *testing.T) {
	c, src, backfilled := backfilledHierarchy(t)
	live := &connector.Recorder{}
	if code := deliver(t, c.Handler(gateFor(src, c, live)), "issues", hook(t, "issues.edited.sub_issue")); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	events := live.Events()
	if len(events) != 1 {
		t.Fatalf("webhook emitted %v, want one", nativeIDs(events))
	}
	var same connector.Event
	for _, b := range backfilled.Events() {
		if b.ID == events[0].ID {
			same = b
		}
	}
	if a, b := asJSON(t, events[0]), asJSON(t, same); a != b {
		t.Errorf("webhook event differs from the backfilled one:\n%s\nwant\n%s", a, b)
	}
}

func pullEvent(t *testing.T, events []connector.Event) connector.Event {
	t.Helper()
	var found []connector.Event
	for _, ev := range events {
		if ev.Kind == connector.KindPullRequest {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d pull request events, want one: %v", len(found), nativeIDs(found))
	}
	return found[0]
}

func backfilledPull(t *testing.T) connector.Event {
	t.Helper()
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, rec))
	return pullEvent(t, rec.Events())
}

func TestAPullRequestCarriesThePathsItTouches(t *testing.T) {
	pr := backfilledPull(t)
	if !slices.Equal(pr.Payload.Paths, pullPaths) || pr.Payload.PathsTruncated {
		t.Errorf("paths = %v, truncated %v; want %v", pr.Payload.Paths, pr.Payload.PathsTruncated, pullPaths)
	}
}

func TestAPullRequestHoldsNoPatch(t *testing.T) {
	if raw := asJSON(t, backfilledPull(t)); strings.Contains(raw, "SECRET_PATCH_LINE") || strings.Contains(raw, "@@") {
		t.Errorf("the event holds a patch: %s", raw)
	}
}

func TestAPullRequestLinksToTheCodeItTouches(t *testing.T) {
	pr := backfilledPull(t)
	api := config.SourceRef{Source: sourceID, Project: "acme/api"}
	code := []config.CodeEntity{
		{ID: "code:acme/api:engine/server", Name: "Engine server", PathPatterns: []string{"engine/server/**"}, Repo: api},
		{ID: "code:acme/api:engine/client", Name: "Engine client", PathPatterns: []string{"engine/client/**"}, Repo: api},
	}
	var systems []string
	for _, ref := range l1.References([]connector.Event{pr}, nil, code) {
		if ref.Type == l1.RefSystem {
			systems = append(systems, ref.ID)
		}
	}
	if want := []string{"code:acme/api:engine/server"}; !slices.Equal(systems, want) {
		t.Errorf("system references = %v, want %v", systems, want)
	}
}

const pushedAt = "2026-09-05T09:00:00Z"

type pullUpdate struct {
	gh    *fakeGitHub
	src   connector.SourceConfig
	c     *github.Connector
	old   connector.Event
	body  []byte
	event connector.Event
}

func updatePull(t *testing.T) *pullUpdate {
	t.Helper()
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	before := &connector.Recorder{}
	backfillAll(t, c, gateFor(src, c, before))
	u := &pullUpdate{gh: gh, src: src, c: c, old: pullEvent(t, before.Events())}

	gh.setList("/repos/acme/api/pulls/2/files", []map[string]any{
		{"filename": "engine/client/y.go", "status": "modified", "patch": "@@ -1 +1 @@"},
	})
	gh.touch("/repos/acme/api/pulls", 2, pushedAt)
	u.body = bytes.ReplaceAll(hook(t, "pull_request.synchronize"), []byte(`"updated_at": "2026-09-04T12:00:00Z"`), []byte(`"updated_at": "`+pushedAt+`"`))
	live := &connector.Recorder{}
	if code := deliver(t, c.Handler(gateFor(src, c, live)), "pull_request", u.body); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	u.event = pullEvent(t, live.Events())
	return u
}

func TestAPullRequestUpdateRefreshesItsPaths(t *testing.T) {
	u := updatePull(t)
	if want := []string{"engine/client/y.go"}; !slices.Equal(u.event.Payload.Paths, want) {
		t.Errorf("paths after the push = %v, want %v", u.event.Payload.Paths, want)
	}
}

func TestAPullRequestUpdateIsANewRevision(t *testing.T) {
	u := updatePull(t)
	if !strings.HasPrefix(u.event.NativeID, "acme/api#2@"+pushedAt+"+paths:") || u.event.NativeID == u.old.NativeID {
		t.Errorf("native id after the push = %s, want a new revision at %s", u.event.NativeID, pushedAt)
	}
}

func TestABackfillAfterAPullRequestUpdateEqualsTheDelivery(t *testing.T) {
	u := updatePull(t)
	after := &connector.Recorder{}
	fresh := newConnector(t, u.src)
	backfillAll(t, fresh, gateFor(u.src, fresh, after))
	if a, b := asJSON(t, pullEvent(t, after.Events())), asJSON(t, u.event); a != b {
		t.Errorf("the backfill after the push differs from the delivery:\n%s\nwant\n%s", a, b)
	}
}

func TestAChangedFilesListUnderTheSameUpdatedAtIsANewRevision(t *testing.T) {
	u := updatePull(t)
	u.gh.setList("/repos/acme/api/pulls/2/files", []map[string]any{{"filename": "engine/client/z.go", "status": "added"}})
	again := &connector.Recorder{}
	if code := deliver(t, u.c.Handler(gateFor(u.src, u.c, again)), "pull_request", u.body); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	if moved := pullEvent(t, again.Events()); moved.NativeID == u.event.NativeID {
		t.Errorf("native id %s is the same for two files lists", moved.NativeID)
	}
}

type boundedRun struct {
	events []connector.Event
	reads  []int
	names  []string
}

func runBoundedPaths(t *testing.T, files int) boundedRun {
	t.Helper()
	items := make([]map[string]any, files)
	names := make([]string, files)
	for i := range items {
		names[i] = fmt.Sprintf("engine/f%04d.go", (i*7919)%files)
		items[i] = map[string]any{"filename": names[i], "status": "modified", "patch": "@@ -1 +1 @@"}
	}
	gh := newFakeGitHub(t)
	gh.setList("/repos/acme/api/pulls/2/files", items)
	src := newSource(t, gh, sourceID, nil)

	run := boundedRun{names: names}
	for range 2 {
		c := newConnector(t, src)
		rec := &connector.Recorder{}
		mark := len(gh.seen())
		backfillAll(t, c, gateFor(src, c, rec))
		run.events = append(run.events, pullEvent(t, rec.Events()))
		reads := 0
		for _, uri := range gh.seen()[mark:] {
			if strings.HasPrefix(uri, "/repos/acme/api/pulls/2/files") {
				reads++
			}
		}
		run.reads = append(run.reads, reads)
	}
	c := newConnector(t, src)
	live := &connector.Recorder{}
	if code := deliver(t, c.Handler(gateFor(src, c, live)), "pull_request", hook(t, "pull_request.synchronize")); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	run.events = append(run.events, pullEvent(t, live.Events()))
	return run
}

var boundedCases = []struct {
	name          string
	files         int
	wantTruncated bool
}{
	{name: "exactly the bound", files: connector.MaxPaths},
	{name: "over the bound", files: connector.MaxPaths + 57, wantTruncated: true},
}

func TestThePathsOfAPullRequestAreBoundedAndSorted(t *testing.T) {
	for _, tt := range boundedCases {
		t.Run(tt.name, func(t *testing.T) {
			run := runBoundedPaths(t, tt.files)
			want := slices.Sorted(slices.Values(run.names[:connector.MaxPaths]))
			got := run.events[0].Payload
			if !slices.Equal(got.Paths, want) || got.PathsTruncated != tt.wantTruncated {
				t.Errorf("paths = %d, truncated %v; want the %d read, sorted, truncated %v", len(got.Paths), got.PathsTruncated, len(want), tt.wantTruncated)
			}
		})
	}
}

func TestTheFilesListOfAPullRequestIsReadAtMostOnceForTheBound(t *testing.T) {
	for _, tt := range boundedCases {
		t.Run(tt.name, func(t *testing.T) {
			limit := (connector.MaxPaths + 99) / 100
			for _, reads := range runBoundedPaths(t, tt.files).reads {
				if reads > limit {
					t.Errorf("the files list was read %d times, want at most %d", reads, limit)
				}
			}
		})
	}
}

func TestBoundedPathsAreTheSameEventFromEveryRead(t *testing.T) {
	for _, tt := range boundedCases {
		t.Run(tt.name, func(t *testing.T) {
			events := runBoundedPaths(t, tt.files).events
			for _, ev := range events[1:] {
				if a, b := asJSON(t, ev), asJSON(t, events[0]); a != b {
					t.Errorf("a second read differs:\n%s\nwant\n%s", a, b)
				}
			}
		})
	}
}
