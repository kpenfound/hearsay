//go:build integration

package deletion_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/agent"
	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/principal"
)

const chat = "kern"

type chatWorld struct {
	pool  *pgxpool.Pool
	store *l0.Store
	repo  config.Repo
	url   string
}

type slowStore struct {
	*l0.Store
	hold func(artifact string)
}

func (s slowStore) SerializeArtifact(ctx context.Context, source, artifact string, fn func(context.Context, connector.LockedArtifact) error) error {
	return s.Store.SerializeArtifact(ctx, source, artifact, func(ctx context.Context, a connector.LockedArtifact) error {
		if err := fn(ctx, a); err != nil {
			return err
		}
		if s.hold != nil {
			s.hold(artifact)
		}
		return nil
	})
}

func newChatWorld(t *testing.T, hold func(artifact string)) *chatWorld {
	t.Helper()
	t.Setenv("DELETION_KERN_TOKEN", "kern-secret")
	pool := dbtest.Scratch(t, "deletion")
	identity := func(id string) []principal.Identity { return []principal.Identity{{Source: chat, NativeID: id}} }
	principals := []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Identities: identity("kyle")},
		{ID: "kern", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "DELETION_KERN_TOKEN", Identities: identity("kern")},
	}
	src := connector.SourceConfig{ID: chat, Type: conversation.Type, Containers: []string{"kyle"},
		Settings: json.RawMessage(`{"owner":"kyle","agents":["kern"]}`)}
	c, err := conversation.New(src, principals, func(name string) (string, bool) {
		if name == "DELETION_KERN_TOKEN" {
			return "kern-secret", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	store := l0.New(pool)
	gate := connector.NewGate(slowStore{store, hold}, chat, c.Describe(), connector.NewAllowlist(src))
	srv := httptest.NewServer(c.Handler(gate))
	t.Cleanup(srv.Close)
	return &chatWorld{pool: pool, store: store, url: srv.URL,
		repo: config.Repo{Principals: principals, Sources: []connector.SourceConfig{src}}}
}

func (w *chatWorld) post(t *testing.T, exchange, role, text string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"owner": "kyle", "conversation": "c-1", "exchange": exchange, "role": role,
		"time": "2026-11-02T09:00:00Z", "text": text})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer kern-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (w *chatWorld) stored(t *testing.T, exchange, role, text string) string {
	t.Helper()
	status, body := w.post(t, exchange, role, text)
	var out conversation.Accepted
	if status != http.StatusAccepted || json.Unmarshal([]byte(body), &out) != nil {
		t.Fatalf("POST %s/%s = %d %s, want 202", exchange, role, status, body)
	}
	return out.EventID
}

func (w *chatWorld) rows(t *testing.T, artifact string) (all, readable int) {
	t.Helper()
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE deletion IS NULL) FROM l0_events WHERE source = $1 AND artifact = $2`,
		chat, artifact).Scan(&all, &readable); err != nil {
		t.Fatal(err)
	}
	return all, readable
}

func (w *chatWorld) request(id string, artifacts ...string) deletion.Request {
	var sels []deletion.Selector
	for _, a := range artifacts {
		sels = append(sels, deletion.Selector{ArtifactSource: chat, ArtifactID: a})
	}
	return deletion.Request{ID: id, Selectors: sels, Reason: "the owner's delete call", Operator: "kyle"}
}

const (
	ownerWords = "c-1/x-1:owner"
	answerOf   = "c-1/x-1:assistant"
)

func TestAMessageStaysDeletedOnceItIsDeleted(t *testing.T) {
	w := newChatWorld(t, nil)
	w.stored(t, "x-1", "owner", "Let's use Postgres.")
	w.stored(t, "x-1", "owner", "Let's use SQLite.")
	answer := w.stored(t, "x-1", "assistant", "Noted.")

	applied, err := deletion.ApplyRequest(t.Context(), w.pool, w.repo, w.request("", ownerWords))
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Events) != 2 {
		t.Fatalf("deleted %v, want both revisions", applied.Events)
	}
	before := state(t, w.pool)
	for _, text := range []string{"Let's use Postgres.", "Let's use SQLite.", "Something else."} {
		status, body := w.post(t, "x-1", "owner", text)
		if status != http.StatusGone || !strings.Contains(body, `"status":"deleted"`) {
			t.Errorf("POST %q = %d %s, want 410 deleted", text, status, body)
		}
	}
	if got := state(t, w.pool); got != before {
		t.Errorf("a refused post wrote something")
	}
	if status, body := w.post(t, "x-1", "assistant", "Noted."); status != http.StatusAccepted || !strings.Contains(body, answer) {
		t.Errorf("the answer of the exchange = %d %s, want 202 unchanged", status, body)
	}
}

func TestAPostThatOverlapsADeletionLeavesNothingReadable(t *testing.T) {
	t.Run("the deletion first", func(t *testing.T) {
		w := newChatWorld(t, nil)
		w.stored(t, "x-1", "owner", "Let's use Postgres.")
		reached, release := make(chan struct{}), make(chan struct{})
		req := w.request("", ownerWords)
		req.After = func(step string) error {
			if step == deletion.StepWalked {
				close(reached)
				<-release
			}
			return nil
		}
		deleted := make(chan error, 1)
		go func() { _, err := deletion.ApplyRequest(context.Background(), w.pool, w.repo, req); deleted <- err }()
		<-reached

		type reply struct {
			status int
			body   string
		}
		posted := make(chan reply, 1)
		go func() { s, b := w.post(t, "x-1", "owner", "Let's use SQLite."); posted <- reply{s, b} }()
		dbtest.WaitForLockWait(t, w.pool)
		select {
		case r := <-posted:
			t.Fatalf("the post was answered while the deletion held the message: %d %s", r.status, r.body)
		default:
		}
		close(release)
		if err := <-deleted; err != nil {
			t.Fatal(err)
		}
		if r := <-posted; r.status != http.StatusGone {
			t.Errorf("the post = %d %s, want 410", r.status, r.body)
		}
		if all, readable := w.rows(t, ownerWords); all != 1 || readable != 0 {
			t.Errorf("the artifact has %d rows, %d readable, want 1 and 0", all, readable)
		}
	})
}

func TestADeletionThatWaitsForAPostCoversItsRevision(t *testing.T) {
	var (
		armed   sync.Mutex
		active  bool
		reached = make(chan struct{})
		release = make(chan struct{})
	)
	w := newChatWorld(t, func(artifact string) {
		armed.Lock()
		on := active && artifact == ownerWords
		active = false
		armed.Unlock()
		if on {
			close(reached)
			<-release
		}
	})
	w.stored(t, "x-1", "owner", "Let's use Postgres.")
	armed.Lock()
	active = true
	armed.Unlock()

	posted := make(chan int, 1)
	go func() { s, _ := w.post(t, "x-1", "owner", "Let's use SQLite."); posted <- s }()
	<-reached

	deleted := make(chan deletion.Applied, 1)
	go func() {
		a, err := deletion.ApplyRequest(context.Background(), w.pool, w.repo, w.request("", ownerWords))
		if err != nil {
			t.Error(err)
		}
		deleted <- a
	}()
	dbtest.WaitForLockWait(t, w.pool)
	select {
	case <-deleted:
		t.Fatal("the deletion finished while a post held the message")
	default:
	}
	close(release)
	if s := <-posted; s != http.StatusAccepted {
		t.Errorf("the post = %d, want 202", s)
	}
	applied := <-deleted
	if len(applied.Events) != 2 {
		t.Errorf("the deletion covered %v, want the first revision and the one the post wrote", applied.Events)
	}
	if all, readable := w.rows(t, ownerWords); all != 2 || readable != 0 {
		t.Errorf("the artifact has %d rows, %d readable, want 2 and 0", all, readable)
	}
	if status, _ := w.post(t, "x-1", "owner", "Let's use Redis."); status != http.StatusGone {
		t.Errorf("a later post = %d, want 410", status)
	}
}

func TestARequestFailedPartWayDeletesNothing(t *testing.T) {
	w := newChatWorld(t, nil)
	ctx := t.Context()
	first := w.stored(t, "x-1", "owner", "The passphrase is tangerine-otter-42.")
	second := w.stored(t, "x-1", "assistant", "I will not repeat it.")
	for i, ev := range []string{first, second} {
		doc := []string{"one", "two"}[i]
		if _, err := w.pool.Exec(ctx, `INSERT INTO l1_docs(id,kind,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,acl,text,raw_text,body,outcome_kind,artifact_class)
VALUES('l1:kern:' || $1::text,'chat_thread','kern',$1,ARRAY[$2],now(),now(),now(),'[{"kind":"public"}]','text','raw','{}','none','chat_thread')`, doc, ev); err != nil {
			t.Fatal(err)
		}
	}
	before := state(t, w.pool)

	for _, step := range []string{deletion.StepWalked, deletion.StepRecorded, deletion.StepEnqueued} {
		failing := w.request("", ownerWords, answerOf)
		calls := 0
		failing.After = func(s string) error {
			if s == step {
				calls++
				if step != deletion.StepEnqueued || calls == 2 {
					return errors.New("injected failure")
				}
			}
			return nil
		}
		if _, err := deletion.ApplyRequest(ctx, w.pool, w.repo, failing); err == nil || !strings.Contains(err.Error(), "injected failure") {
			t.Fatalf("after %s: ApplyRequest() = %v, want the injected failure", step, err)
		}
		if got := state(t, w.pool); got != before {
			t.Errorf("after %s: a failed request left writes behind", step)
		}
	}

	guarded := w.request("", ownerWords, answerOf)
	guarded.Guard = func(context.Context, pgx.Tx, deletion.Preview) error { return errors.New("not yours") }
	if _, err := deletion.ApplyRequest(ctx, w.pool, w.repo, guarded); err == nil || err.Error() != "not yours" {
		t.Fatalf("ApplyRequest(guarded) = %v, want the guard's refusal", err)
	}
	if got := state(t, w.pool); got != before {
		t.Error("a refused request left writes behind")
	}

	applied, err := deletion.ApplyRequest(ctx, w.pool, w.repo, w.request("", ownerWords, answerOf))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(applied.Events, sorted(first, second)) || len(applied.Documents) != 2 {
		t.Errorf("ApplyRequest() = %+v, want both messages and both documents", applied)
	}
	var records, events int
	if err := w.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM l0_deletions), (SELECT count(*) FROM l0_events WHERE kind = 'deletion')`).Scan(&records, &events); err != nil {
		t.Fatal(err)
	}
	if records != 1 || events != 1 {
		t.Errorf("%d deletion records and %d deletion events, want one of each", records, events)
	}
	if got := state(t, w.pool); strings.Contains(got, "tangerine-otter-42") {
		t.Error("the words are still in L0")
	}
}

func sorted(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func TestARequestUnderARecordedIDIsAnsweredWithTheOriginal(t *testing.T) {
	w := newChatWorld(t, nil)
	ctx := t.Context()
	w.stored(t, "x-1", "owner", "Let's use Postgres.")
	w.stored(t, "x-2", "owner", "And Go.")

	const n = 8
	var wg sync.WaitGroup
	answers := make([]deletion.Applied, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			answers[i], err = deletion.ApplyRequest(ctx, w.pool, w.repo, w.request("del_fixed", ownerWords, "c-1/x-2:owner"))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	replays := 0
	for _, a := range answers {
		if a.Replay {
			replays++
		}
		if a.Deletion != "del_fixed" || !slices.Equal(a.Events, answers[0].Events) || a.Retraction != answers[0].Retraction {
			t.Errorf("an answer differs from the others: %+v", a)
		}
	}
	if replays != n-1 {
		t.Errorf("%d replays, want %d: exactly one request wrote", replays, n-1)
	}
	var records, events int
	if err := w.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM l0_deletions), (SELECT count(*) FROM l0_events WHERE kind = 'deletion')`).Scan(&records, &events); err != nil {
		t.Fatal(err)
	}
	if records != 1 || events != 1 {
		t.Errorf("%d deletion records and %d deletion events, want one of each", records, events)
	}
	later, err := deletion.ApplyRequest(ctx, w.pool, w.repo, w.request("del_fixed", ownerWords, "c-1/x-2:owner"))
	if err != nil || !later.Replay || later.Retraction != answers[0].Retraction {
		t.Errorf("a retry after the answer was lost = %+v, %v, want the original", later, err)
	}
}

func TestAgentSessionPostsAfterAnOperatorDeletionBehaveAsBefore(t *testing.T) {
	t.Setenv("DELETION_SHED_TOKEN", "shed-secret")
	pool := dbtest.Scratch(t, "deletion")
	principals := []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "DELETION_SHED_TOKEN"},
	}
	src := connector.SourceConfig{ID: "sessions", Type: agent.Type, Containers: []string{connector.AllowAll}}
	c, err := agent.New(src, principals, func(name string) (string, bool) { return "shed-secret", name == "DELETION_SHED_TOKEN" })
	if err != nil {
		t.Fatal(err)
	}
	store := l0.New(pool)
	srv := httptest.NewServer(c.Handler(connector.NewGate(store, src.ID, c.Describe(), connector.NewAllowlist(src))))
	t.Cleanup(srv.Close)
	post := func(body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer shed-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	turn := func(n, text string) string {
		return `{"on_behalf_of":"kyle","session":"s-01","kind":"agent_turn","turn":"` + n + `","started_at":"2026-09-23T17:00:00Z","time":"2026-09-23T17:00:05Z","text":"` + text + `"}`
	}
	if got := post(turn("1", "Reading the policy.")); got != http.StatusAccepted {
		t.Fatalf("the first turn = %d, want 202", got)
	}
	repo := config.Repo{Principals: principals, Sources: []connector.SourceConfig{src}}
	if _, err := deletion.Apply(t.Context(), pool, repo, deletion.Selector{ArtifactSource: src.ID, ArtifactID: "s-01"}, "test", "kyle"); err != nil {
		t.Fatal(err)
	}
	count := func() (all, readable int) {
		if err := pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE deletion IS NULL) FROM l0_events WHERE source = $1`, src.ID).Scan(&all, &readable); err != nil {
			t.Fatal(err)
		}
		return all, readable
	}
	if got := post(turn("1", "Reading the policy.")); got != http.StatusAccepted {
		t.Errorf("the covered turn sent again = %d, want 202", got)
	}
	if all, readable := count(); all != 1 || readable != 0 {
		t.Errorf("after the re-send L0 holds %d rows, %d readable, want the one redacted row", all, readable)
	}
	if got := post(turn("2", "Another turn.")); got != http.StatusAccepted {
		t.Errorf("a new turn = %d, want 202", got)
	}
	if all, readable := count(); all != 2 || readable != 1 {
		t.Errorf("after a new turn L0 holds %d rows, %d readable, want 2 and 1", all, readable)
	}
}
