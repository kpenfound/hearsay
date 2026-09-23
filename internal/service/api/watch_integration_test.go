//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// watchWait is how long a watch in these tests waits before it returns empty.
const watchWait = 300 * time.Millisecond

// watchWorld is a database of its own, because a watch reads the whole feed
// and the distiller's cursor, which every other test in the database moves.
// The entities:
//
//	code:acme/w           the repository
//	└─ code:acme/w:engine a directory of it
//	code:acme/other       somewhere else
//
// kyle and sam reach everything, and each may read what is public and what is
// granted to their own identity. ann reaches the engine alone. boss is an
// orchestrator, shed a worker and peek an observer.
type watchWorld struct {
	src, repo, engine, other string
	pool                     *pgxpool.Pool
	calls                    *api.Calls
	events                   *l0.Store
	docs                     *l1.Store
	principals               []principal.Principal
	server                   *httptest.Server
	hour                     int
}

var (
	annWatches  = api.Caller{Principal: "ann"}
	bossForKyle = api.Caller{Principal: "kyle", Agent: "boss"}
	peekForKyle = api.Caller{Principal: "kyle", Agent: "peek"}
	openACL     = connector.ACL{{Kind: connector.ACLPublic}}
)

func newWatchWorld(t *testing.T) *watchWorld {
	t.Helper()
	pool, err := db.Connect(t.Context(), api.ScratchDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w := &watchWorld{src: "w", repo: "code:acme/w", engine: "code:acme/w:engine", other: "code:acme/other",
		pool: pool, events: l0.New(pool), docs: l1.New(pool)}
	graph := l2.New(pool)
	for _, e := range []l2.Entity{
		{ID: w.repo, Type: l2.TypeProject, Name: "watch-repo", Origin: l2.OriginConfig},
		{ID: w.engine, Type: l2.TypeModule, Name: "watch-engine", PartOf: []string{w.repo}, Origin: l2.OriginConfig},
		{ID: w.other, Type: l2.TypeProject, Name: "watch-other", Origin: l2.OriginConfig},
	} {
		if err := graph.PutEntity(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	w.principals = []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.AllScopes()},
			Identities: []principal.Identity{{Source: w.src, NativeID: kyleNode}}},
		{ID: "sam", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.AllScopes()},
			Identities: []principal.Identity{{Source: w.src, NativeID: "sam-node"}}},
		{ID: "ann", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.SomeScopes(w.engine)},
			Identities: []principal.Identity{{Source: w.src, NativeID: "ann-node"}}},
		{ID: "boss", Kind: principal.KindAgent, Class: principal.ClassOrchestrator, Grant: principal.Grant{Scopes: principal.AllScopes()}},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, Grant: principal.Grant{Scopes: principal.AllScopes()}},
		{ID: "peek", Kind: principal.KindAgent, Class: principal.ClassObserver, Grant: principal.Grant{Scopes: principal.AllScopes()}},
	}
	// The credentials reachWorld.post sends.
	for i := range w.principals {
		env := "HEARSAY_WATCH_" + strings.ToUpper(w.principals[i].ID) + "_TOKEN"
		w.principals[i].TokenEnv = env
		t.Setenv(env, "reach-"+w.principals[i].ID+"-credential")
	}
	w.calls, err = api.NewCalls(pool, config.Repo{Principals: w.principals}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.calls.WithWatch(watchWait, 20*time.Millisecond)
	w.server = httptest.NewServer(api.Handler(w.calls, pool))
	t.Cleanup(w.server.Close)
	return w
}

// emit appends an event and returns its id. Its title is words no
// notification may carry.
func (w *watchWorld) emit(t *testing.T, source, artifact string, kind connector.Kind, acl connector.ACL) string {
	t.Helper()
	return w.emitIn(t, w.events, source, artifact, kind, acl)
}

// emitIn is emit in a transaction of the caller's.
func (w *watchWorld) emitIn(t *testing.T, events *l0.Store, source, artifact string, kind connector.Kind, acl connector.ACL) string {
	t.Helper()
	w.hour++
	ev := connector.Event{
		Source: source, NativeID: artifact, Kind: kind, Time: day.Add(time.Duration(w.hour) * time.Hour),
		Payload: connector.Payload{
			Artifact: artifact, Title: "private words about " + artifact, Text: "private words",
			Container: connector.Container{Kind: connector.ContainerRepository, NativeID: "acme/w"},
			Author:    &connector.Identity{Source: source, Kind: connector.IdentityUser, NativeID: kyleNode},
		},
		ACL: acl,
	}
	if _, err := events.Append(t.Context(), ev); err != nil {
		t.Fatalf("Append(%s) = %v", artifact, err)
	}
	return connector.EventID(source, artifact)
}

// distil writes the document an artifact's events are distilled into, about
// these entities, as the distiller would.
func (w *watchWorld) distil(t *testing.T, source, artifact string, scope []string, acl connector.ACL, events ...string) string {
	t.Helper()
	at := day.Add(time.Duration(w.hour) * time.Hour)
	doc := l1.Document{
		ID: l1.DocID(source, artifact), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
		Source: l1.Source{System: source, NativeID: artifact}, L0Refs: events,
		Time:  l1.Times{Created: at, Updated: at, LastActivity: at},
		Scope: scope, ACL: acl, Text: "private words", RawText: "private words",
		Body: l1.Body{Summary: "private words", OutcomeKind: l1.OutcomeNone},
	}
	if _, err := w.docs.Put(t.Context(), doc); err != nil {
		t.Fatalf("Put(%s) = %v", doc.ID, err)
	}
	return doc.ID
}

// settled waits until the feed serves everything this database has
// committed. The feed holds back behind any transaction open in the cluster,
// and the other packages' tests share the cluster, so without this a test
// would race them.
func (w *watchWorld) settled(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var behind int
		if err := w.pool.QueryRow(t.Context(),
			`SELECT count(*) FROM l0_events WHERE xact_id >= pg_snapshot_xmin(pg_current_snapshot())`).Scan(&behind); err != nil {
			t.Fatal(err)
		}
		if behind == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the feed is still %d events behind after 30s", behind)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pumped waits for the feed to settle and moves the distiller's cursor to its
// end: it has read everything there is.
func (w *watchWorld) pumped(t *testing.T) {
	t.Helper()
	w.settled(t)
	var last string
	if err := w.pool.QueryRow(t.Context(),
		`SELECT coalesce((SELECT xact_id::text || '.' || seq FROM l0_events ORDER BY xact_id DESC, seq DESC LIMIT 1), '')`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	head, err := l0.ParseCursor(last)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l0.NewCursors(w.pool).Save(t.Context(), distiller.Consumer, head); err != nil {
		t.Fatal(err)
	}
}

// both makes one call over HTTP and over MCP.
func (w *watchWorld) both(t *testing.T, caller api.Caller, args map[string]any) (overHTTP, overMCP answer) {
	t.Helper()
	return (&reachWorld{server: w.server}).both(t, caller, "watch", args)
}

// watch calls watch over both interfaces, fails the test unless both served
// the same bytes and succeeded, and decodes them.
func (w *watchWorld) watch(t *testing.T, caller api.Caller, args map[string]any) api.Watched {
	t.Helper()
	overHTTP, overMCP := w.both(t, caller, args)
	if overHTTP.status != http.StatusOK || overMCP.isError {
		t.Fatalf("watch(%v) for %+v: HTTP %d %s, MCP isError=%v %s", args, caller, overHTTP.status, overHTTP.body, overMCP.isError, overMCP.body)
	}
	if overHTTP.body != overMCP.body {
		t.Fatalf("watch(%v) for %+v differs:\nHTTP %s\nMCP  %s", args, caller, overHTTP.body, overMCP.body)
	}
	if strings.Contains(overHTTP.body, "private words") {
		t.Errorf("a notification carries content: %s", overHTTP.body)
	}
	var got api.Watched
	if err := json.Unmarshal([]byte(overHTTP.body), &got); err != nil || got.Notifications == nil || got.Cursor == "" {
		t.Fatalf("watch served %s (%v): want notifications and a cursor", overHTTP.body, err)
	}
	return got
}

func eventsOf(notes []api.Notification) []string {
	out := []string{}
	for _, n := range notes {
		out = append(out, n.Event)
	}
	return out
}

// An event is delivered once it is distilled, and not before: the watch holds
// its cursor at an event the distiller has not read, and at one whose job has
// not run, rather than pass it.
func TestWatchDeliversAnEventOnceItIsDistilled(t *testing.T) {
	w := newWatchWorld(t)
	w.distil(t, w.src, "acme/w#0", []string{w.repo}, openACL, w.emit(t, w.src, "acme/w#0", connector.KindIssue, openACL))
	w.pumped(t)
	start := w.watch(t, kyle, map[string]any{"scope": w.repo})
	if len(start.Notifications) != 0 {
		t.Fatalf("a watch from now returned what was there before it: %v", start.Notifications)
	}
	ev := w.emit(t, w.src, "acme/w#1", connector.KindIssue, openACL)
	args := map[string]any{"scope": w.repo, "after": start.Cursor}
	if got := w.watch(t, kyle, args); len(got.Notifications) != 0 || got.Cursor != start.Cursor {
		t.Fatalf("before the distiller read it: %+v, want nothing and the cursor %s", got, start.Cursor)
	}
	if _, err := queue.Enqueue(t.Context(), w.pool, queue.Request{Kind: distiller.JobKind(), TargetID: l1.DocID(w.src, "acme/w#1")}); err != nil {
		t.Fatal(err)
	}
	w.pumped(t)
	if got := w.watch(t, kyle, args); len(got.Notifications) != 0 || got.Cursor != start.Cursor {
		t.Fatalf("while its job is still to run: %+v, want nothing and the cursor %s", got, start.Cursor)
	}

	// Distilled, about an entity under the scope.
	doc := w.distil(t, w.src, "acme/w#1", []string{w.engine}, openACL, ev)
	got := w.watch(t, kyle, args)
	want := []api.Notification{{Event: ev, Kind: connector.KindIssue, Source: w.src, Time: day.Add(2 * time.Hour), Document: doc}}
	if len(got.Notifications) != 1 || got.Notifications[0].Event != want[0].Event || got.Notifications[0].Kind != want[0].Kind ||
		got.Notifications[0].Source != want[0].Source || !got.Notifications[0].Time.Equal(want[0].Time) || got.Notifications[0].Document != want[0].Document {
		t.Fatalf("after distillation: %+v, want %+v", got.Notifications, want)
	}
	if again := w.watch(t, kyle, map[string]any{"scope": w.repo, "after": got.Cursor}); len(again.Notifications) != 0 {
		t.Errorf("after the cursor it returned: %+v, want nothing", again.Notifications)
	}

	// A job that finished without putting the event in any document is not
	// waited for: the watch passes the event.
	w.emit(t, w.src, "acme/w#2", connector.KindIssue, openACL)
	w.pumped(t)
	skipped := w.watch(t, kyle, map[string]any{"scope": w.repo, "after": got.Cursor})
	if len(skipped.Notifications) != 0 || skipped.Cursor == got.Cursor {
		t.Errorf("an event distilled into nothing: %+v, want nothing and a cursor past it", skipped)
	}
}

// A watch from now passes over what had committed before it, even where an
// open transaction holds the feed short of it, and delivers what that
// transaction writes once it commits: the event was not there at the start.
func TestAWatchFromNowPassesWhatHadCommittedBehindAnOpenTransaction(t *testing.T) {
	w := newWatchWorld(t)
	tx, err := w.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	late := w.emitIn(t, l0.New(tx), w.src, "acme/w#1", connector.KindIssue, openACL)
	before := w.emit(t, w.src, "acme/w#2", connector.KindIssue, openACL)
	start := w.watch(t, kyle, map[string]any{"scope": w.repo})
	if len(start.Notifications) != 0 {
		t.Fatalf("a watch from now returned %v", start.Notifications)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	w.distil(t, w.src, "acme/w#1", []string{w.repo}, openACL, late)
	w.distil(t, w.src, "acme/w#2", []string{w.repo}, openACL, before)
	w.pumped(t)
	if got := eventsOf(w.watch(t, kyle, map[string]any{"scope": w.repo, "after": start.Cursor}).Notifications); !slices.Equal(got, []string{late}) {
		t.Errorf("got %v, want %v alone: %s had committed before the watch began", got, []string{late}, before)
	}
}

// Reading on from the cursor each watch returns delivers every event on the
// scope once, in feed order, whatever the feed holds between them and however
// many watches it takes.
func TestWatchResumesFromItsCursorWithoutDuplicatesOrGaps(t *testing.T) {
	w := newWatchWorld(t)
	start := w.watch(t, kyle, map[string]any{"scope": w.repo})
	var want []string
	add := func(n int) {
		t.Helper()
		for range n {
			artifact := fmt.Sprintf("acme/w#%d", w.hour+1)
			ev := w.emit(t, w.src, artifact, connector.KindIssue, openACL)
			w.distil(t, w.src, artifact, []string{w.repo}, openACL, ev)
			want = append(want, ev)
			// Something on another scope between every two.
			elsewhere := fmt.Sprintf("acme/other#%d", w.hour+1)
			w.distil(t, w.src, elsewhere, []string{w.other}, openACL, w.emit(t, w.src, elsewhere, connector.KindIssue, openACL))
		}
		w.pumped(t)
	}
	// A page of the feed's worth of events nobody distilled, so that the
	// first page read holds some notifications but fewer than a batch.
	filler := func(n int) {
		t.Helper()
		for range n {
			w.emit(t, w.src, fmt.Sprintf("acme/other#%d", w.hour+1), connector.KindIssue, openACL)
		}
	}
	add(api.MaxNotifications / 2)
	filler(l0.MaxLimit)
	add(api.MaxNotifications/2 + 5)
	var got []string
	cursor := start.Cursor
	for _, size := range []int{api.MaxNotifications, 5, 0} {
		batch := w.watch(t, kyle, map[string]any{"scope": w.repo, "after": cursor})
		if len(batch.Notifications) != size {
			t.Fatalf("watch after %d notifications returned %d, want %d", len(got), len(batch.Notifications), size)
		}
		got, cursor = append(got, eventsOf(batch.Notifications)...), batch.Cursor
	}
	add(3)
	got = append(got, eventsOf(w.watch(t, kyle, map[string]any{"scope": w.repo, "after": cursor}).Notifications)...)
	if !slices.Equal(got, want) {
		t.Errorf("delivered %d events, want the %d on the scope in order:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
}

// The filter narrows by kind and by source, and an empty one is everything.
func TestWatchFiltersByKindAndSource(t *testing.T) {
	w := newWatchWorld(t)
	start := w.watch(t, kyle, map[string]any{"scope": w.repo})
	put := func(source, artifact string, kind connector.Kind) string {
		t.Helper()
		ev := w.emit(t, source, artifact, kind, openACL)
		w.distil(t, source, artifact, []string{w.repo}, openACL, ev)
		return ev
	}
	issue := put(w.src, "acme/w#1", connector.KindIssue)
	pr := put(w.src, "acme/w#2", connector.KindPullRequest)
	elsewhere := put("w2", "acme/w#3", connector.KindIssue)
	w.pumped(t)
	for _, tt := range []struct {
		name   string
		filter map[string]any
		want   []string
	}{
		{"no filter", nil, []string{issue, pr, elsewhere}},
		{"empty lists", map[string]any{"kinds": []string{}, "sources": []string{}}, []string{issue, pr, elsewhere}},
		{"a kind", map[string]any{"kinds": []string{"pull_request"}}, []string{pr}},
		{"a source", map[string]any{"sources": []string{"w2"}}, []string{elsewhere}},
		{"both", map[string]any{"kinds": []string{"issue"}, "sources": []string{w.src}}, []string{issue}},
		{"a kind nothing is", map[string]any{"kinds": []string{"comment"}}, []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]any{"scope": w.repo, "after": start.Cursor}
			if tt.filter != nil {
				args["filter"] = tt.filter
			}
			if got := eventsOf(w.watch(t, kyle, args).Notifications); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// Only an agent that may subscribe may watch — orchestrator and above — and
// a person calling directly. The refusal is the same bytes over both.
func TestWatchIsForOrchestratorsAndPeople(t *testing.T) {
	w := newWatchWorld(t)
	for _, tt := range []struct {
		name   string
		caller api.Caller
		want   int
	}{
		{"a person", kyle, http.StatusOK},
		{"an orchestrator", bossForKyle, http.StatusOK},
		{"a worker", shedForKyle, http.StatusForbidden},
		{"an observer", peekForKyle, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			overHTTP, overMCP := w.both(t, tt.caller, map[string]any{"scope": w.repo})
			if overHTTP.status != tt.want || overMCP.isError != (tt.want != http.StatusOK) || overHTTP.body != overMCP.body {
				t.Errorf("HTTP %d %s, MCP isError=%v %s; want %d over both with the same body",
					overHTTP.status, overHTTP.body, overMCP.isError, overMCP.body, tt.want)
			}
		})
	}
}

// What a reader may not read — by the event's access list, the document's, or
// reach — is never delivered to them, and Hearsay's own events and what is
// never distilled are delivered to nobody, even when a document names them.
// A notification carries its five handles and nothing that says where it was
// in the feed.
func TestWatchNeverShowsWhatTheReaderMayNotRead(t *testing.T) {
	w := newWatchWorld(t)
	start := w.watch(t, kyle, map[string]any{"scope": w.repo})
	kyleOnly := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}}
	put := func(source, artifact string, kind connector.Kind, eventACL, docACL connector.ACL, scope string) string {
		t.Helper()
		ev := w.emit(t, source, artifact, kind, eventACL)
		w.distil(t, source, artifact, []string{scope}, docACL, ev)
		return ev
	}
	private := put(w.src, "acme/w#1", connector.KindIssue, kyleOnly, kyleOnly, w.engine)
	privateEvent := put(w.src, "acme/w#2", connector.KindIssue, kyleOnly, openACL, w.engine)
	privateDoc := put(w.src, "acme/w#3", connector.KindIssue, openACL, kyleOnly, w.engine)
	repoOnly := put(w.src, "acme/w#4", connector.KindIssue, openACL, openACL, w.repo)
	put(w.src, "acme/w#5", connector.KindToolCall, openACL, openACL, w.engine)
	put(w.src, "acme/w#6", connector.KindAudit, openACL, openACL, w.engine)
	put(connector.SelfSource, "acme/w#7", connector.KindIssue, openACL, openACL, w.engine)
	// An artifact deleted at the source: neither it nor its tombstone.
	put(w.src, "acme/w#9", connector.KindIssue, openACL, openACL, w.engine)
	w.hour++
	if _, err := w.events.Append(t.Context(), connector.Event{
		Source: w.src, NativeID: "acme/w#9:tombstone", Kind: connector.KindTombstone, Time: day.Add(time.Duration(w.hour) * time.Hour),
		Payload: connector.Payload{Artifact: "acme/w#9:tombstone", Target: "acme/w#9",
			Container: connector.Container{Kind: connector.ContainerRepository, NativeID: "acme/w"}},
		ACL: openACL,
	}); err != nil {
		t.Fatal(err)
	}
	engine := put(w.src, "acme/w#8", connector.KindIssue, openACL, openACL, w.engine)
	w.pumped(t)
	// And a bundle served, which is an audit event of Hearsay's own.
	(&reachWorld{server: w.server}).both(t, kyle, "get_bundle", map[string]any{"scope": w.repo})

	for _, tt := range []struct {
		name   string
		caller api.Caller
		scope  string
		want   []string
	}{
		{"kyle", kyle, w.repo, []string{private, privateEvent, privateDoc, repoOnly, engine}},
		{"kyle's orchestrator", bossForKyle, w.repo, []string{private, privateEvent, privateDoc, repoOnly, engine}},
		{"sam", sam, w.repo, []string{repoOnly, engine}},
		{"ann, who reaches the engine", annWatches, w.repo, []string{engine}},
		{"ann on the engine", annWatches, w.engine, []string{engine}},
		{"kyle on the engine", kyle, w.engine, []string{private, privateEvent, privateDoc, engine}},
		{"kyle elsewhere", kyle, w.other, []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			overHTTP, _ := w.both(t, tt.caller, map[string]any{"scope": tt.scope, "after": start.Cursor})
			var shape struct {
				Notifications []map[string]any `json:"notifications"`
			}
			if err := json.Unmarshal([]byte(overHTTP.body), &shape); err != nil {
				t.Fatal(err)
			}
			for _, n := range shape.Notifications {
				if len(n) != 5 || n["event"] == nil || n["kind"] == nil || n["source"] == nil || n["time"] == nil || n["document"] == nil {
					t.Errorf("a notification is %v: want its event, kind, source, time and document and nothing else", n)
				}
			}
			got := w.watch(t, tt.caller, map[string]any{"scope": tt.scope, "after": start.Cursor})
			if ids := eventsOf(got.Notifications); !slices.Equal(ids, tt.want) {
				t.Errorf("got %v, want %v", ids, tt.want)
			}
		})
	}
}

// With nothing to deliver a watch waits, then returns an empty list; it holds
// no database connection while it waits, and ends when its request does.
func TestWatchWaitsThenReturnsEmpty(t *testing.T) {
	w := newWatchWorld(t)
	began := time.Now()
	first := w.watch(t, kyle, map[string]any{"scope": w.repo})
	if elapsed := time.Since(began); elapsed < 2*watchWait {
		t.Errorf("two empty watches took %v, want each to wait %v", elapsed, watchWait)
	}
	if len(first.Notifications) != 0 {
		t.Errorf("an empty watch returned %v", first.Notifications)
	}
	if again := w.watch(t, kyle, map[string]any{"scope": w.repo, "after": first.Cursor}); again.Cursor != first.Cursor {
		t.Errorf("with nothing on the feed the cursor moved from %s to %s", first.Cursor, again.Cursor)
	}
	if overHTTP, overMCP := w.both(t, kyle, map[string]any{"scope": w.repo, "after": "not-a-cursor"}); overHTTP.status != http.StatusBadRequest || overHTTP.body != overMCP.body {
		t.Errorf("a cursor no watch returned: HTTP %d %s, MCP %s; want a 400 and the same body", overHTTP.status, overHTTP.body, overMCP.body)
	}

	// A pool of its own, so what it holds is the watch's alone.
	pool, err := db.Connect(t.Context(), w.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	calls, err := api.NewCalls(pool, config.Repo{Principals: w.principals}, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls.WithWatch(time.Minute, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := calls.Call(ctx, kyle, "watch", json.RawMessage(`{"scope":"`+w.repo+`"}`))
		done <- err
	}()
	for range 5 {
		time.Sleep(100 * time.Millisecond)
		if held := pool.Stat().AcquiredConns(); held != 0 {
			t.Errorf("a waiting watch holds %d connections", held)
		}
	}
	cancel()
	select {
	case err := <-done:
		var e *api.Error
		if !errors.As(err, &e) {
			t.Errorf("a cancelled watch returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a watch did not end within a second of its request being cancelled")
	}
}

// A server that is stopping ends the watches waiting on it with what they
// have, rather than hold its shutdown for their wait.
func TestAWaitingWatchEndsWhenTheServerStops(t *testing.T) {
	w := newWatchWorld(t)
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan error, 1)
	go func() {
		stopped <- api.Run(ctx, &config.Config{Repo: config.Repo{Principals: w.principals}}, api.Deps{Pool: w.pool, Listener: listener})
	}()
	answered := make(chan string, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+listener.Addr().String()+"/v1/watch",
			strings.NewReader(`{"scope":"`+w.repo+`"}`))
		req.Header.Set(api.PrincipalHeader, "kyle")
		req.Header.Set(api.AuthorizationHeader, "Bearer reach-kyle-credential")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		answered <- fmt.Sprintf("%d %s", resp.StatusCode, body)
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s with a watch waiting")
	}
	if got := <-answered; !strings.HasPrefix(got, `200 {"notifications":[]`) {
		t.Errorf("the waiting watch was answered %s, want an empty list", got)
	}
}
