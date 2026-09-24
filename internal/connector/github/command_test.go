package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantOK   bool
		wantName string
		wantArgs []string
	}{
		{name: "ratify", body: "/hearsay ratify", wantOK: true, wantName: "ratify"},
		{name: "a command name in any case", body: "/hearsay Demote", wantOK: true, wantName: "demote"},
		{name: "after blank lines, with lines after it", body: "\n  \n/hearsay pin\r\nThis is the one.", wantOK: true, wantName: "pin"},
		{name: "merge with two topic ids, backticks taken off", body: "/hearsay merge `topic:a1`  topic:b2", wantOK: true, wantName: "merge", wantArgs: []string{"topic:a1", "topic:b2"}},
		{name: "the word alone", body: "/hearsay", wantOK: true},
		{name: "an unknown command is still a command", body: "/hearsay frobnicate now", wantOK: true, wantName: "frobnicate", wantArgs: []string{"now"}},
		{name: "not at the start", body: "Should we /hearsay ratify this?", wantOK: false},
		{name: "not the first line", body: "LGTM\n/hearsay ratify", wantOK: false},
		{name: "a longer word", body: "/hearsayratify", wantOK: false},
		{name: "another spelling", body: "/Hearsay ratify", wantOK: false},
		{name: "nothing", body: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, args, ok := github.ParseCommand(tt.body)
			if ok != tt.wantOK || name != tt.wantName || !slices.Equal(args, tt.wantArgs) {
				t.Errorf("ParseCommand(%q) = %q, %q, %v, want %q, %q, %v", tt.body, name, args, ok, tt.wantName, tt.wantArgs, tt.wantOK)
			}
		})
	}
}

// commentHook is an issue_comment delivery for a comment on issue or pull
// request n, built from the fixture.
func commentHook(t *testing.T, action string, n int, id int64, body, created, updated string) []byte {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(hook(t, "issue_comment.created"), &d); err != nil {
		t.Fatal(err)
	}
	d["action"] = action
	d["comment"] = commentItem(n, id, body, created, updated)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func commentItem(n int, id int64, body, created, updated string) map[string]any {
	return map[string]any{
		"url":        fmt.Sprintf("https://api.github.com/repos/acme/api/issues/comments/%d", id),
		"html_url":   fmt.Sprintf("https://github.com/acme/api/issues/%d#issuecomment-%d", n, id),
		"issue_url":  fmt.Sprintf("https://api.github.com/repos/acme/api/issues/%d", n),
		"id":         id,
		"user":       map[string]any{"login": "robinok", "id": 2, "node_id": "MDQ6VXNlcjI=", "type": "User"},
		"created_at": created,
		"updated_at": updated,
		"body":       body,
	}
}

// A `/hearsay` comment on an issue or a pull request is a `command`, with the
// command read from it; Hearsay's reply to one is a `github.reply`, based on
// `command`, whatever it says, so its webhook echo is never a command and
// never team content; anything else is a `message`. In a read-only source a
// `/hearsay` comment is a `message` too, what the person wrote and nothing
// more, and a reply is still a reply. A backfill that reads the same comment
// emits the same event (ADR-0022).
func TestCommentCommandsAndRepliesAreControlTraffic(t *testing.T) {
	const at = "2026-09-24T12:00:00Z"
	tests := []struct {
		name     string
		n        int
		body     string
		updated  string
		readOnly bool
		wantKind connector.Kind
		wantBase connector.Kind
		// wantNative is the event's native, as JSON.
		wantNative string
	}{
		{name: "ratify on an issue", n: 1, body: "/hearsay ratify", wantKind: connector.KindCommand,
			wantNative: `{"id":5001,"repository":"acme/api","issue":1,"command":"ratify"}`},
		{name: "merge on a pull request", n: 2, body: "/hearsay merge `topic:a` topic:b", wantKind: connector.KindCommand,
			wantNative: `{"id":5001,"repository":"acme/api","issue":2,"command":"merge","args":["topic:a","topic:b"]}`},
		{name: "a malformed command is a command", n: 1, body: "/hearsay", wantKind: connector.KindCommand,
			wantNative: `{"id":5001,"repository":"acme/api","issue":1,"command":""}`},
		{name: "an edited command says so", n: 1, body: "/hearsay demote", updated: "2026-09-24T12:05:00Z", wantKind: connector.KindCommand,
			wantNative: `{"id":5001,"repository":"acme/api","issue":1,"command":"demote","edited":true}`},
		{name: "a comment that mentions the command", n: 1, body: "Try /hearsay ratify here.", wantKind: connector.KindMessage,
			wantNative: `{"id":5001}`},
		{name: "Hearsay's reply", n: 1, body: "Ratified 2 stances.\n\n" + github.ReplyMarker(4999), wantKind: github.KindReply, wantBase: connector.KindCommand,
			wantNative: `{"id":5001,"reply_to":4999}`},
		{name: "a reply that starts like a command is still a reply", n: 2, body: "/hearsay ratify\n\n" + github.ReplyMarker(4999), wantKind: github.KindReply, wantBase: connector.KindCommand,
			wantNative: `{"id":5001,"reply_to":4999}`},
		{name: "ratify in a read-only source is a message", n: 1, body: "/hearsay ratify", readOnly: true, wantKind: connector.KindMessage,
			wantNative: `{"id":5001}`},
		{name: "merge on a pull request in a read-only source is a message", n: 2, body: "/hearsay merge `topic:a` topic:b", readOnly: true, wantKind: connector.KindMessage,
			wantNative: `{"id":5001}`},
		{name: "Hearsay's reply in a read-only source is still a reply", n: 1, body: "Ratified 2 stances.\n\n" + github.ReplyMarker(4999), readOnly: true, wantKind: github.KindReply, wantBase: connector.KindCommand,
			wantNative: `{"id":5001,"reply_to":4999}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated := tt.updated
			if updated == "" {
				updated = at
			}
			gh := newFakeGitHub(t)
			src := newSource(t, gh, sourceID, nil)
			src.ReadOnly = tt.readOnly
			c := newConnector(t, src)
			if got := slices.Contains(c.Describe().Kinds, connector.KindCommand); got == tt.readOnly {
				t.Errorf("Describe() declares command: %v, want %v", got, !tt.readOnly)
			}
			live := &connector.Recorder{}
			if code := deliver(t, c.Handler(gateFor(src, c, live)), "issue_comment", commentHook(t, "created", tt.n, 5001, tt.body, at, updated)); code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", code)
			}
			events := live.Events()
			if len(events) != 1 {
				t.Fatalf("events = %s, want one", asJSON(t, events))
			}
			ev := events[0]
			wantParent := fmt.Sprintf("acme/api#%d", tt.n)
			if ev.Kind != tt.wantKind || ev.Payload.BaseKind != tt.wantBase || string(ev.Payload.Native) != tt.wantNative ||
				ev.Payload.Parent != wantParent || ev.Payload.Thread != wantParent || ev.Payload.Text != tt.body || ev.Payload.Author == nil {
				t.Errorf("event = %s, want kind %q base %q native %s on %s", asJSON(t, ev), tt.wantKind, tt.wantBase, tt.wantNative, wantParent)
			}
			if tt.wantKind == connector.KindCommand {
				cmd, err := github.CommandOf(ev)
				if err != nil || cmd.Comment != 5001 || cmd.Issue != tt.n || cmd.Repo != "acme/api" {
					t.Errorf("CommandOf() = %+v, %v", cmd, err)
				}
			} else if _, err := github.CommandOf(ev); err == nil {
				t.Errorf("CommandOf(a %s) = nil error, want a refusal", ev.Kind)
			}

			gh.setList("/repos/acme/api/issues/comments", []map[string]any{commentItem(tt.n, 5001, tt.body, at, updated)})
			backfilled := &connector.Recorder{}
			backfillAll(t, c, gateFor(src, c, backfilled))
			i := slices.IndexFunc(backfilled.Events(), func(b connector.Event) bool { return b.ID == ev.ID })
			if i < 0 {
				t.Fatalf("the backfill did not emit %s", ev.NativeID)
			}
			if a, b := asJSON(t, ev), asJSON(t, backfilled.Events()[i]); a != b {
				t.Errorf("webhook event differs from the backfilled one:\n%s\nwant\n%s", a, b)
			}
		})
	}
}

// Deleting a command comment is a tombstone for it, like any comment's.
func TestADeletedCommandIsATombstone(t *testing.T) {
	gh := newFakeGitHub(t)
	src := newSource(t, gh, sourceID, nil)
	c := newConnector(t, src)
	rec := &connector.Recorder{}
	const at = "2026-09-24T12:00:00Z"
	if code := deliver(t, c.Handler(gateFor(src, c, rec)), "issue_comment", commentHook(t, "deleted", 1, 5001, "/hearsay ratify", at, at)); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	events := rec.Events()
	if len(events) != 1 || events[0].Kind != connector.KindTombstone || events[0].Payload.Target != "acme/api#1:comment:5001" {
		t.Errorf("events = %s, want the command comment's tombstone", asJSON(t, events))
	}
}

// fakeComments is the part of GitHub's REST API a replier calls: one
// issue's comments, which it lists, posts to and edits.
type fakeComments struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	comments []map[string]any
	next     int64
	posts    int
	patches  int
	queries  []string
}

func newFakeComments(t *testing.T) *fakeComments {
	f := &fakeComments{t: t, next: 7000}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeComments) serve(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		f.t.Errorf("%s %s carries Authorization %q, want the configured token", r.Method, r.URL.Path, got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var in struct {
		Body string `json:"body"`
	}
	if r.Method != http.MethodGet {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &in); err != nil {
			f.t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/issues/1/comments":
		f.queries = append(f.queries, r.URL.RawQuery)
		writeJSON(w, f.comments)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/issues/1/comments":
		f.posts++
		f.next++
		c := map[string]any{"id": f.next, "body": in.Body}
		f.comments = append(f.comments, c)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, c)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/api/issues/comments/"):
		f.patches++
		for _, c := range f.comments {
			if fmt.Sprint(c["id"]) == strings.TrimPrefix(r.URL.Path, "/repos/acme/api/issues/comments/") {
				c["body"] = in.Body
				writeJSON(w, c)
				return
			}
		}
		http.NotFound(w, r)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	}
}

// A read-only source has no replier, whatever its token could do: Hearsay
// posts nothing to it.
func TestNoReplierForAReadOnlySource(t *testing.T) {
	f := newFakeComments(t)
	r, err := github.NewReplier(connector.SourceConfig{
		ID: sourceID, Type: github.Type, Containers: []string{"acme/api"}, ReadOnly: true,
		Settings: json.RawMessage(`{"api_url":"` + f.srv.URL + `"}`),
		Secrets:  map[string]string{github.SecretToken: testToken},
	})
	if err == nil || !strings.Contains(err.Error(), "read_only") {
		t.Errorf("NewReplier(a read-only source) = %v, %v, want a refusal naming read_only", r, err)
	}
}

func newReplier(t *testing.T, f *fakeComments) *github.Replier {
	t.Helper()
	r, err := github.NewReplier(connector.SourceConfig{
		ID: sourceID, Type: github.Type, Containers: []string{"acme/api"},
		Settings: json.RawMessage(`{"api_url":"` + f.srv.URL + `"}`),
		Secrets:  map[string]string{github.SecretToken: testToken},
	})
	if err != nil {
		t.Fatalf("NewReplier() = %v", err)
	}
	return r
}

// A reply is posted once: a second call finds the one the first posted by the
// marker naming its command, and posts nothing. It is revised in place, and a
// reply somebody deleted is ErrReplyGone.
func TestReplierPostsOneReplyPerCommand(t *testing.T) {
	f := newFakeComments(t)
	r := newReplier(t, f)
	since := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	f.comments = []map[string]any{{"id": 6000, "body": "Ratified.\n\n" + github.ReplyMarker(4000)}}

	id, err := r.Reply(t.Context(), "acme/api", 1, 5001, since, "Ratified 1 stance.")
	if err != nil {
		t.Fatalf("Reply() = %v", err)
	}
	again, err := r.Reply(t.Context(), "acme/api", 1, 5001, since, "Ratified 1 stance.")
	if err != nil || again != id {
		t.Fatalf("Reply(again) = %d, %v, want %d: the reply already there", again, err, id)
	}
	if f.posts != 1 || id != 7001 {
		t.Fatalf("posts = %d, id = %d, want one reply, 7001: another command's reply is not this one's", f.posts, id)
	}
	if body := f.comments[1]["body"]; body != "Ratified 1 stance.\n\n"+github.ReplyMarker(5001) {
		t.Errorf("reply = %q, want the text and the marker", body)
	}
	if len(f.queries) == 0 || !strings.Contains(f.queries[0], "since=2026-09-24T11%3A59%3A00Z") {
		t.Errorf("queries = %q, want comments since a minute before the command", f.queries)
	}

	if err := r.Revise(t.Context(), "acme/api", 5001, id, "Ratified 1 stance.\n\nUndone."); err != nil {
		t.Fatalf("Revise() = %v", err)
	}
	if body := f.comments[1]["body"]; body != "Ratified 1 stance.\n\nUndone.\n\n"+github.ReplyMarker(5001) {
		t.Errorf("revised reply = %q", body)
	}
	if err := r.Revise(t.Context(), "acme/api", 5001, 9999, "gone"); !errors.Is(err, github.ErrReplyGone) {
		t.Errorf("Revise(a deleted reply) = %v, want ErrReplyGone", err)
	}
	if _, err := r.Reply(t.Context(), "acme/other", 1, 5001, since, "x"); err == nil {
		t.Error("Reply() in a repository the source does not name = nil, want a refusal")
	}
	if f.posts != 1 || f.patches != 2 {
		t.Errorf("posts = %d, patches = %d, want 1 and 2", f.posts, f.patches)
	}
}
