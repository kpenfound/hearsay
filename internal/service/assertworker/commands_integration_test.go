//go:build integration

package assertworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

// fakeReplier is GitHub's side of the replies: one comment per command it is
// asked to answer, found again rather than posted twice, as github.Replier
// does.
type fakeReplier struct {
	mu      sync.Mutex
	replies map[int64]int64  // command comment → reply comment
	bodies  map[int64]string // reply comment → text
	posts   int
	revises int
	fail    error
	gone    bool
}

func newFakeReplier() *fakeReplier {
	return &fakeReplier{replies: map[int64]int64{}, bodies: map[int64]string{}}
}

func (f *fakeReplier) Reply(_ context.Context, repo string, issue int, command int64, _ time.Time, body string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return 0, f.fail
	}
	if id, ok := f.replies[command]; ok {
		return id, nil
	}
	f.posts++
	id := 9000 + int64(len(f.replies))
	f.replies[command], f.bodies[id] = id, body
	return id, nil
}

func (f *fakeReplier) Revise(_ context.Context, repo string, command, reply int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.revises++
	if f.gone {
		return fmt.Errorf("revising: %w", github.ErrReplyGone)
	}
	f.bodies[reply] = body
	return nil
}

// reply is the text of the reply to a command comment, and false where there
// is none.
func (f *fakeReplier) reply(command int64) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.replies[command]
	return f.bodies[id], ok
}

// commands is a GitHub source, its configuration and its L0 and L1 state for
// running commands against: the fixture's issue #12 and pull request #31,
// distilled; two topics a stance from the issue and one from the pull request
// open; and a third topic kyle may not read, from a document only u9 may.
type commands struct {
	t       *testing.T
	pool    *pgxpool.Pool
	src     string
	repo    config.Repo
	a       *assertworker.Asserter
	replies *fakeReplier
	f       fixture
	// topics are the two readable topics, and hidden the one kyle may not
	// read.
	topics [2]string
	hidden string
	next   int64
}

// commandConfig is the configuration the commands run under: kyle and sam
// are people, only kyle may ratify by hand, and bot is an agent.
func commandConfig(t *testing.T, src string) config.Repo {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"sources/github.yaml": "id: " + src + "\ntype: github\ncontainers: [" + repo + "]\n",
		"scopes/s.yaml":       "id: " + src + "\nsources: [" + src + "]\n",
		"principals/p.yaml": "- id: kyle\n  identities: [{source: " + src + ", native_id: u1, handle: kpenfound}]\n" +
			"- id: sam\n  identities: [{source: " + src + ", native_id: u2, handle: samr}]\n" +
			"- id: bot\n  kind: agent\n  class: orchestrator\n  scopes: ['*']\n  token_env: BOT_TOKEN\n  identities: [{source: " + src + ", native_id: u3}]\n",
		"authority/a.yaml": "scope: \"*\"\nratified_by:\n  principals: [kyle]\n",
	}
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	return repo
}

func newCommands(t *testing.T, pool *pgxpool.Pool) *commands {
	t.Helper()
	src := newSource(t)
	c := &commands{t: t, pool: pool, src: src, repo: commandConfig(t, src), replies: newFakeReplier(), f: store(t, pool, src), next: 5000}
	cfg := config.Default()
	cfg.Repo = c.repo
	registry, err := llm.NewFake(llm.Default(), llm.NewFixtures())
	if err != nil {
		t.Fatal(err)
	}
	a, err := assertworker.New(pool, registry, &cfg)
	if err != nil {
		t.Fatalf("assertworker.New() = %v", err)
	}
	c.a = a.WithReplies(assertworker.Replies{src: c.replies})

	secret := event(src, connector.KindIssue, repo+"#40", at(1), "someone", "u9", "a private matter", "Only u9 may read this.")
	secret.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "u9"}}
	if _, err := l0.New(pool).Append(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	secretDoc := buildDoc(t, src, secret, nil, issueBody)
	if _, err := l1.New(pool).Put(t.Context(), secretDoc); err != nil {
		t.Fatal(err)
	}
	graph := l2.New(pool)
	open := func(i int, doc l1.Document, name string) string {
		t.Helper()
		topic := l2.Topic{ID: l2.TopicID(src, doc.ID, i, name), Scope: src, Name: name, ACL: doc.ACL, OpenedBy: doc.ID}
		if _, err := graph.OpenTopic(t.Context(), topic); err != nil {
			t.Fatal(err)
		}
		st := l2.Stance{ID: l2.StanceID(topic.ID, doc.ID, name, at(i), l2.TierInferred), TopicID: topic.ID, Position: name,
			StatedAt: at(i), Evidence: []string{doc.ID}, Tier: l2.TierInferred, ACL: doc.ACL}
		if _, _, err := graph.AppendStance(t.Context(), st, at(i)); err != nil {
			t.Fatal(err)
		}
		return topic.ID
	}
	issue, pr := documents(t, src)
	c.topics = [2]string{open(0, issue, "where the lock goes"), open(1, pr, "when the lock is taken")}
	c.hidden = open(2, secretDoc, "a private matter")
	return c
}

// comment appends a comment on issue or pull request n as the GitHub
// connector emits it, by the GitHub user nativeID, and returns its event.
func (c *commands) comment(n int, nativeID, body string) connector.Event {
	c.t.Helper()
	c.next++
	return c.revision(n, c.next, nativeID, body, at(10), at(10), "")
}

// revision appends one revision of comment id on n, with a suffix to its
// revision token such as a private repository's.
func (c *commands) revision(n int, id int64, nativeID, body string, created, updated time.Time, suffix string) connector.Event {
	c.t.Helper()
	parent := fmt.Sprintf("%s#%d", repo, n)
	artifact := fmt.Sprintf("%s:comment:%d", parent, id)
	token := updated.Format(time.RFC3339) + suffix
	ev := event(c.src, connector.KindMessage, artifact, created, "", nativeID, "", body)
	ev.NativeID = artifact + "@" + token
	ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: updated}
	ev.Payload.Parent, ev.Payload.Thread = parent, parent
	ev.Payload.Native = json.RawMessage(fmt.Sprintf(`{"id":%d}`, id))
	if name, args, ok := github.ParseCommand(body); ok {
		raw, err := json.Marshal(github.Command{Comment: id, Repo: repo, Issue: n, Name: name, Args: args, Edited: updated.After(created)})
		if err != nil {
			c.t.Fatal(err)
		}
		ev.Kind, ev.Payload.Native = connector.KindCommand, raw
	}
	return c.append(ev)
}

// delete appends the tombstone of a comment.
func (c *commands) delete(comment connector.Event) connector.Event {
	c.t.Helper()
	artifact := comment.Payload.Artifact + ":tombstone"
	return c.append(connector.Event{
		Source: c.src, NativeID: artifact, Kind: connector.KindTombstone, Time: comment.Time, ACL: comment.ACL,
		Payload: connector.Payload{Artifact: artifact, Target: comment.Payload.Artifact, Container: comment.Payload.Container},
	})
}

func (c *commands) append(ev connector.Event) connector.Event {
	c.t.Helper()
	if _, err := l0.New(c.pool).Append(c.t.Context(), ev); err != nil {
		c.t.Fatalf("Append(%s) = %v", ev.NativeID, err)
	}
	ev.ID = connector.EventID(ev.Source, ev.NativeID)
	return ev
}

// handle runs the job the follower enqueues for an event, under the scope's
// key.
func (c *commands) handle(ev connector.Event) error {
	return c.a.Handle(c.t.Context(), queue.Job{Kind: l2.AssertKind(), TargetID: l2.GestureTarget + ev.ID, SerialKey: c.src, Attempt: queue.DefaultMaxAttempts})
}

func (c *commands) mustHandle(ev connector.Event) {
	c.t.Helper()
	if err := c.handle(ev); err != nil {
		c.t.Fatalf("Handle(%s) = %v", ev.ID, err)
	}
}

func (c *commands) gestures() []l2.Gesture {
	c.t.Helper()
	gs, err := l2.New(c.pool).Gestures(c.t.Context(), c.src)
	if err != nil {
		c.t.Fatal(err)
	}
	return gs
}

func (c *commands) operations() []l2.Operation {
	c.t.Helper()
	ops, err := l2.New(c.pool).Operations(c.t.Context(), l2.OperationFilter{Scope: c.src})
	if err != nil {
		c.t.Fatal(err)
	}
	return ops
}

func commentID(t *testing.T, ev connector.Event) int64 {
	t.Helper()
	cmd, err := github.CommandOf(ev)
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Comment
}

// Each command, run by the person it names, on an issue or a pull request,
// does what it says or refuses, and is answered with one reply saying which
// and how to undo it. A refusal changes nothing.
func TestGitHubCommands(t *testing.T) {
	pool := newPool(t)
	tests := []struct {
		name string
		n    int
		// author is the GitHub user's node id: u1 is kyle, u2 sam, who may
		// not ratify by hand, u3 an agent and u8 nobody Hearsay knows.
		author string
		// body is the comment; {a} and {b} are the readable topics and
		// {hidden} the one kyle may not read.
		body       string
		wantReply  []string
		wantAction l2.GestureAction
		wantMerge  bool
	}{
		{name: "ratify on an issue", n: 12, author: "u1", body: "/hearsay ratify", wantAction: l2.GestureRatify,
			wantReply: []string{"Ratified 1 stance drawn from this issue in scope", "delete your `/hearsay ratify` comment", "hearsay gestures undo"}},
		{name: "demote on a pull request", n: 31, author: "u1", body: "/hearsay demote", wantAction: l2.GestureDemote,
			wantReply: []string{"Demoted 1 stance drawn from this pull request", "contested", "delete your `/hearsay demote` comment"}},
		{name: "pin an issue", n: 12, author: "u1", body: "/hearsay pin", wantAction: l2.GesturePin,
			wantReply: []string{"Pinned this issue as an anchor in scope", "hearsay gestures undo"}},
		{name: "merge two topics", n: 12, author: "u1", body: "/hearsay merge {a} `{b}`", wantMerge: true,
			wantReply: []string{"Merged topic `{a}` into topic `{b}`", "delete your `/hearsay merge` comment", "hearsay topics undo"}},
		{name: "an unknown command", n: 12, author: "u1", body: "/hearsay frobnicate",
			wantReply: []string{"Hearsay has no such command", "changed nothing"}},
		{name: "the word alone", n: 12, author: "u1", body: "/hearsay",
			wantReply: []string{"Hearsay has no such command"}},
		{name: "ratify with an argument", n: 12, author: "u1", body: "/hearsay ratify {a}",
			wantReply: []string{"`/hearsay ratify` takes nothing after it"}},
		{name: "merge with one topic", n: 12, author: "u1", body: "/hearsay merge {a}",
			wantReply: []string{"`/hearsay merge` takes two different topic ids"}},
		{name: "an unmapped GitHub account", n: 12, author: "u8", body: "/hearsay ratify",
			wantReply: []string{"not mapped to a Hearsay principal", "changed nothing"}},
		{name: "an agent's account", n: 12, author: "u3", body: "/hearsay pin",
			wantReply: []string{"maps to `bot`, which is not a person", "Only a person"}},
		{name: "a person ratified_by leaves out", n: 12, author: "u2", body: "/hearsay ratify",
			wantReply: []string{"You may not ratify this issue", `"sam" may not ratify by hand`, "changed nothing"}},
		{name: "a person ratified_by leaves out cannot merge", n: 12, author: "u2", body: "/hearsay merge {a} {b}",
			wantReply: []string{"You may not merge these topics"}},
		{name: "merge a topic the commenter may not read", n: 12, author: "u1", body: "/hearsay merge {hidden} {a}",
			wantReply: []string{"no topic `{hidden}` that you can read", "merged nothing"}},
		{name: "merge into a topic that does not exist", n: 12, author: "u1", body: "/hearsay merge {a} topic:nope",
			wantReply: []string{"no topic `topic:nope` that you can read"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCommands(t, pool)
			ids := strings.NewReplacer("{a}", c.topics[0], "{b}", c.topics[1], "{hidden}", c.hidden)
			ev := c.comment(tt.n, tt.author, ids.Replace(tt.body))
			c.mustHandle(ev)

			got, ok := c.replies.reply(commentID(t, ev))
			if !ok || c.replies.posts != 1 {
				t.Fatalf("replies posted = %d, reply = %q, want one", c.replies.posts, got)
			}
			for _, want := range tt.wantReply {
				if want = ids.Replace(want); !strings.Contains(got, want) {
					t.Errorf("reply = %q, want it to say %q", got, want)
				}
			}
			gs, ops := c.gestures(), c.operations()
			switch {
			case tt.wantAction != "":
				if len(gs) != 1 {
					t.Fatalf("gestures = %+v, want one %s", gs, tt.wantAction)
				}
				if gs[0].Action != tt.wantAction || gs[0].Event != ev.ID || gs[0].Principal != "kyle" || gs[0].Documents[0] != l1.DocID(c.src, fmt.Sprintf("%s#%d", repo, tt.n)) {
					t.Errorf("gestures = %+v, want one %s by kyle from %s", gs, tt.wantAction, ev.ID)
				}
				if !strings.Contains(got, fmt.Sprintf("as gesture %d", gs[0].ID)) {
					t.Errorf("reply = %q, want it to name gesture %d", got, gs[0].ID)
				}
			case tt.wantMerge:
				if len(ops) != 1 {
					t.Fatalf("operations = %+v, want one merge", ops)
				}
				if ops[0].Kind != l2.OperationMerge || ops[0].Topics[0] != c.topics[1] || ops[0].Topics[1] != c.topics[0] {
					t.Errorf("operations = %+v, want %s merged into %s", ops, c.topics[0], c.topics[1])
				}
			}
			if tt.wantAction == "" && len(gs) != 0 {
				t.Errorf("gestures = %+v, want none", gs)
			}
			if !tt.wantMerge && len(ops) != 0 {
				t.Errorf("operations = %+v, want none", ops)
			}
			for _, secret := range []string{"where the lock goes", "when the lock is taken", "a private matter"} {
				if strings.Contains(got, secret) {
					t.Errorf("reply = %q quotes the topic name %q, which not everyone who reads the issue may read", got, secret)
				}
			}
		})
	}
}

// A command runs once and is answered once: a retried job posts no second
// reply and records no second gesture, a reply that could not be posted is
// posted by the retry, and an edit of a command runs nothing.
func TestAGitHubCommandRunsOnceAndIsAnsweredOnce(t *testing.T) {
	c := newCommands(t, newPool(t))
	ev := c.comment(12, "u1", "/hearsay ratify")
	c.replies.fail = errors.New("GitHub is down")
	if err := c.handle(ev); err == nil {
		t.Fatal("Handle() with GitHub down = nil, want the error that retries it")
	}
	if gs := c.gestures(); len(gs) != 1 {
		t.Fatalf("gestures = %+v, want the ratify recorded before the reply", gs)
	}
	c.replies.fail = nil
	c.mustHandle(ev)
	c.mustHandle(ev)
	if gs := c.gestures(); len(gs) != 1 || c.replies.posts != 1 {
		t.Fatalf("gestures = %d, replies = %d, want one of each", len(gs), c.replies.posts)
	}

	id := commentID(t, ev)
	for _, edit := range []connector.Event{
		c.revision(12, id, "u1", "/hearsay demote", at(10), at(11), ""),
		// The repository went private: a new revision of the same comment,
		// not an edit, and it is the same command, which has run.
		c.revision(12, id, "u1", "/hearsay ratify", at(10), at(10), "+perm:private"),
	} {
		c.mustHandle(edit)
	}
	edited := c.revision(12, 7777, "u1", "/hearsay demote", at(10), at(11), "")
	c.mustHandle(edited)
	if gs := c.gestures(); len(gs) != 1 || c.replies.posts != 1 {
		t.Errorf("gestures = %+v, replies = %d, want only the ratify: an edit is not run", gs, c.replies.posts)
	}
}

// A comment that is not a command, and Hearsay's own reply echoed back by the
// webhook, are not commands: the follower puts neither on the queue, and a
// job for one does nothing.
func TestOnlyCommandsAreRun(t *testing.T) {
	c := newCommands(t, scratchPool(t))
	message := c.comment(12, "u1", "Let's /hearsay ratify this later.")
	echo := event(c.src, github.KindReply, repo+"#12:comment:6100", at(10), "hearsay[bot]", "b1", "", "/hearsay ratify\n\n"+github.ReplyMarker(5001))
	echo.Payload.BaseKind = connector.KindCommand
	echo.Payload.Parent, echo.Payload.Thread = repo+"#12", repo+"#12"
	echo.Payload.Native = json.RawMessage(`{"id":6100,"reply_to":5001}`)
	echo = c.append(echo)
	for _, ev := range []connector.Event{message, echo} {
		c.mustHandle(ev)
	}
	follower := assertworker.NewCommandFollower(c.pool, c.repo, 0, 0)
	if _, err := follower.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := c.pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job WHERE target_id LIKE 'gesture:%'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 || c.replies.posts != 0 || len(c.gestures()) != 0 {
		t.Errorf("queued = %d, replies = %d, gestures = %d, want nothing for a comment and an echo", queued, c.replies.posts, len(c.gestures()))
	}
}

// The follower enqueues each command as written, under the scope it acts on —
// for a merge, its topics' — and the tombstone of each deleted one under the
// same key; not an edit, not another source's comment, and not a deleted
// comment that was not a command.
func TestTheCommandFollowerEnqueuesCommandsAndTheirDeletions(t *testing.T) {
	pool := scratchPool(t)
	c := newCommands(t, pool)
	graph := l2.New(pool)
	elsewhere := [2]string{}
	for i := range elsewhere {
		issue, _ := documents(t, c.src)
		topic := l2.Topic{ID: l2.TopicID("elsewhere", issue.ID, i, "t"), Scope: "elsewhere", Name: "t", ACL: issue.ACL, OpenedBy: issue.ID}
		if _, err := graph.OpenTopic(t.Context(), topic); err != nil {
			t.Fatal(err)
		}
		elsewhere[i] = topic.ID
	}
	ratify := c.comment(12, "u1", "/hearsay ratify")
	merge := c.comment(31, "u1", "/hearsay merge "+elsewhere[0]+" "+elsewhere[1])
	edited := c.revision(12, 6000, "u1", "/hearsay pin", at(10), at(11), "")
	message := c.comment(12, "u1", "Agreed.")

	follower := assertworker.NewCommandFollower(pool, c.repo, 0, 0)
	if _, err := follower.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	deleted := []connector.Event{c.delete(ratify), c.delete(merge), c.delete(message)}
	if _, err := follower.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		l2.GestureTarget + ratify.ID:     c.src,
		l2.GestureTarget + merge.ID:      "elsewhere",
		l2.GestureTarget + deleted[0].ID: c.src,
		l2.GestureTarget + deleted[1].ID: "elsewhere",
	}
	rows, err := pool.Query(t.Context(), `SELECT target_id, serial_key FROM queue_job WHERE target_id LIKE 'gesture:%'`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var target, key string
		if err := rows.Scan(&target, &key); err != nil {
			t.Fatal(err)
		}
		got[target] = key
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("queued = %v, want %v (not %s, an edit, nor %s, a comment)", got, want, edited.ID, deleted[2].ID)
	}
}

// Deleting a command comment undoes what it did — a gesture, or a merge — and
// the one reply is revised to say so. Applying the deletion again does
// nothing more.
func TestDeletingACommandUndoesIt(t *testing.T) {
	for _, body := range []string{"/hearsay ratify", "/hearsay pin", "/hearsay merge {a} {b}"} {
		t.Run(body, func(t *testing.T) {
			c := newCommands(t, newPool(t))
			ev := c.comment(12, "u1", strings.NewReplacer("{a}", c.topics[0], "{b}", c.topics[1]).Replace(body))
			c.mustHandle(ev)
			tomb := c.delete(ev)
			c.mustHandle(tomb)
			c.mustHandle(tomb)

			reply, _ := c.replies.reply(commentID(t, ev))
			if c.replies.posts != 1 || c.replies.revises != 1 {
				t.Errorf("posts = %d, revises = %d, want one reply, revised once", c.replies.posts, c.replies.revises)
			}
			if gs := c.gestures(); len(gs) > 0 {
				if len(gs) != 2 || gs[1].Action != l2.GestureUndo || gs[1].Event != tomb.ID || gs[0].UndoneBy != gs[1].ID {
					t.Errorf("gestures = %+v, want the gesture undone by the tombstone", gs)
				}
				if want := fmt.Sprintf("the command comment was deleted, so Hearsay undid gesture %d, as gesture %d.", gs[0].ID, gs[1].ID); !strings.Contains(reply, want) {
					t.Errorf("reply = %q, want it to say %q", reply, want)
				}
				return
			}
			ops := c.operations()
			if len(ops) != 2 || ops[1].Kind != l2.OperationUndo || ops[0].UndoneBy != ops[1].ID {
				t.Fatalf("operations = %+v, want the merge undone", ops)
			}
			if want := fmt.Sprintf("so Hearsay undid operation %d, as operation %d.", ops[0].ID, ops[1].ID); !strings.Contains(reply, want) {
				t.Errorf("reply = %q, want it to say %q", reply, want)
			}
			if !strings.HasPrefix(reply, "Merged topic") {
				t.Errorf("reply = %q, want it to keep what the command did", reply)
			}
		})
	}
}

// A merge a later operation builds on is not undone when its command is
// deleted, and the one reply says why and what to undo first. A command that
// was refused has nothing to undo, and its reply is not revised; a reply
// somebody deleted is not replaced.
func TestUndoingAMergeALaterOperationDependsOnIsRefused(t *testing.T) {
	c := newCommands(t, newPool(t))
	issue, _ := documents(t, c.src)
	third := l2.Topic{ID: l2.TopicID(c.src, issue.ID, 9, "third"), Scope: c.src, Name: "third", ACL: issue.ACL, OpenedBy: issue.ID}
	if _, err := l2.New(c.pool).OpenTopic(t.Context(), third); err != nil {
		t.Fatal(err)
	}
	merge := c.comment(12, "u1", "/hearsay merge "+c.topics[0]+" "+c.topics[1])
	c.mustHandle(merge)
	later, err := l2.Operate(t.Context(), c.pool, c.repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", From: c.topics[1], Into: third.ID})
	if err != nil {
		t.Fatalf("Operate(a later merge) = %v", err)
	}
	c.mustHandle(c.delete(merge))
	ops := c.operations()
	if len(ops) != 2 || ops[0].UndoneBy != 0 {
		t.Errorf("operations = %+v, want both merges still in force", ops)
	}
	reply, _ := c.replies.reply(commentID(t, merge))
	want := fmt.Sprintf("but Hearsay could not undo operation %d, because later operations on its topics are still in force: operation %d. Undo those first", ops[0].ID, later.ID)
	if !strings.Contains(reply, want) || c.replies.posts != 1 || c.replies.revises != 1 {
		t.Errorf("reply = %q (posts %d, revises %d), want one reply revised to say %q", reply, c.replies.posts, c.replies.revises, want)
	}

	refused := c.comment(12, "u2", "/hearsay ratify")
	c.mustHandle(refused)
	c.mustHandle(c.delete(refused))
	if c.replies.revises != 1 {
		t.Errorf("revises = %d, want a refused command's reply left alone", c.replies.revises)
	}

	pin := c.comment(12, "u1", "/hearsay pin")
	c.mustHandle(pin)
	c.replies.gone = true
	tomb := c.delete(pin)
	c.mustHandle(tomb)
	c.mustHandle(tomb)
	if c.replies.posts != 3 || c.replies.revises != 2 {
		t.Errorf("posts = %d, revises = %d, want no new reply for one somebody deleted", c.replies.posts, c.replies.revises)
	}
}

// A command deleted before it ran runs nothing, and its deletion finds nothing
// to undo.
func TestACommandDeletedBeforeItRanRunsNothing(t *testing.T) {
	c := newCommands(t, newPool(t))
	ev := c.comment(12, "u1", "/hearsay ratify")
	tomb := c.delete(ev)
	c.mustHandle(ev)
	c.mustHandle(tomb)
	if gs := c.gestures(); len(gs) != 0 || c.replies.posts != 0 {
		t.Errorf("gestures = %+v, replies = %d, want nothing", gs, c.replies.posts)
	}
}

// A ratify of an issue whose document is still being distilled waits for it
// rather than answering that there is nothing to ratify, until its last
// attempt.
func TestACommandWaitsForItsDocument(t *testing.T) {
	// Its own database: another package's distiller would take the job.
	c := newCommands(t, scratchPool(t))
	fresh := event(c.src, connector.KindIssue, repo+"#77", at(9), "kpenfound", "u1", "a new issue", "Nothing distilled yet.")
	c.append(fresh)
	doc := l1.DocID(c.src, repo+"#77")
	if _, err := queue.Enqueue(t.Context(), c.pool, queue.Request{Kind: queue.Kind{Name: "distill"}, TargetID: doc}); err != nil {
		t.Fatal(err)
	}
	ev := c.comment(77, "u1", "/hearsay ratify")
	job := queue.Job{Kind: l2.AssertKind(), TargetID: l2.GestureTarget + ev.ID, SerialKey: c.src, Attempt: 1}
	if err := c.a.Handle(t.Context(), job); err == nil || c.replies.posts != 0 {
		t.Fatalf("Handle(first attempt) = %v, posts = %d, want a retry and no reply", err, c.replies.posts)
	}
	c.mustHandle(ev)
	if reply, _ := c.replies.reply(commentID(t, ev)); !strings.Contains(reply, "has not distilled this conversation") {
		t.Errorf("reply = %q, want it to say the issue is not distilled yet", reply)
	}
}
