//go:build integration

package connectors_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/service/connectors"
)

// newPool connects to the database the integration-test check brings up.
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

// newSourceID is a source id nothing else in the suite uses: the database
// outlives one test and the packages run in parallel, so an assertion is scoped
// to a source rather than to a table.
func newSourceID(t *testing.T) string {
	t.Helper()
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "conn"
}

// pager records the cursor every Backfill call was given, which is how a
// restarted process is asked where it resumed from.
type pager struct {
	*connector.Fake

	mu   sync.Mutex
	from []connector.Cursor
}

func (p *pager) Backfill(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.BackfillResult, error) {
	p.mu.Lock()
	p.from = append(p.from, from)
	p.mu.Unlock()
	return p.Fake.Backfill(ctx, sink, from)
}

func (p *pager) cursors() []connector.Cursor {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.from)
}

// The service against the real thing: what a connector polls, backfills and is
// pushed lands in L0, the backfill's position is in the database, and a second
// process started against the same database does not walk the history again.
func TestTheServiceIngestsIntoL0AndKeepsItsPosition(t *testing.T) {
	pool := newPool(t)
	id := newSourceID(t)
	src := connector.SourceConfig{ID: id, Type: connector.FakeType, Containers: []string{"C123"}, Refresh: time.Millisecond}
	cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}
	store := l0.New(pool)
	cursors := l0.NewBackfillCursors(pool)

	fake := connector.NewFake(src)
	fake.Queue = []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "polled")}
	fake.Pages = [][]connector.Event{
		{fake.NewEvent(connector.KindMessage, "h1", "history one")},
		{fake.NewEvent(connector.KindMessage, "h2", "history two")},
	}
	first := &pager{Fake: fake}

	addr, stop := run(t, cfg, connectors.Deps{
		Pool:     pool,
		Registry: fakeRegistry(t, map[string]connector.Connector{id: first}),
	})

	// The push path, through the mounted handler and the same gate.
	body, err := json.Marshal(fake.NewEvent(connector.KindMessage, "p1", "pushed"))
	if err != nil {
		t.Fatalf("marshalling the event: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+addr+connector.HookPath(id), strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST the hook: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing the response body: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s = %d, want 202", connector.HookPath(id), resp.StatusCode)
	}

	waitFor(t, "everything the connector produced to reach L0", func() bool {
		return len(artifactsIn(t, store, id)) == 4
	})
	if got, want := artifactsIn(t, store, id), []string{"h1", "h2", "m1", "p1"}; !slices.Equal(got, want) {
		t.Errorf("L0 holds %v, want the polled, backfilled and pushed artifacts %v", got, want)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	state, err := cursors.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if !state.Done || state.Events != 2 {
		t.Errorf("the stored position is %+v, want done after two events", state)
	}

	// A second process against the same database. History is walked once: the
	// position says it is done, so the restarted process asks for no page at
	// all.
	second := &pager{Fake: connector.NewFake(src)}
	second.Pages = fake.Pages
	_, stop = run(t, cfg, connectors.Deps{
		Pool:     pool,
		Registry: fakeRegistry(t, map[string]connector.Connector{id: second}),
	})
	time.Sleep(50 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatalf("Run(second) = %v, want nil", err)
	}
	if got := second.cursors(); len(got) != 0 {
		t.Errorf("the restarted process asked for pages %v, want none: the backfill was done", got)
	}
	if got := first.cursors(); len(got) == 0 || got[0] != "" {
		t.Errorf("the first process started the backfill at %v, want the zero cursor", got)
	}
}

// Ready is not the same as up: a process whose database has gone away is
// running and cannot ingest (ADR-0008).
func TestReadyzFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	newPool(t) // skips the test when there is no database

	// A pool of its own, closed: that is a database this process cannot reach,
	// without taking the one every other test is using away from them.
	pool, err := db.Connect(t.Context(), os.Getenv("HEARSAY_DATABASE_URL"))
	if err != nil {
		t.Fatalf("Connect() = %v, want no error", err)
	}
	pool.Close()

	addr, stop := run(t, &config.Config{}, connectors.Deps{
		Pool:    pool,
		Sink:    &connector.Recorder{},
		Cursors: connector.NewMemoryCursors(),
	})
	code, body := get(t, addr, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with an unreachable database = %d, want 503: %s", code, body)
	}
	// What went wrong is in the log. The body says that something is, and names
	// no host, no user and no password.
	for _, secret := range []string{"127.0.0.1", "hearsay@", "closed"} {
		if strings.Contains(body, secret) {
			t.Errorf("the readiness body carries %q, which is the operator's to read in the log: %s", secret, body)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

// A database this binary is ahead of is one this process cannot ingest into,
// and ADR-0008 makes that a readiness failure — which is what turns ADR-0006's
// version check into a deployment that stops rather than one that half-works.
func TestReadyzFailsWhenTheSchemaIsBehindTheBinary(t *testing.T) {
	newPool(t) // skips the test when there is no database

	// The maintenance database every Postgres server has, which nothing
	// migrates. db.Open rather than db.Connect: Connect is the startup check
	// and would refuse it here, which is the other half of the same rule.
	pool, err := db.Open(t.Context(), swapDatabaseName(os.Getenv("HEARSAY_DATABASE_URL"), "postgres"))
	if err != nil {
		t.Fatalf("Open() = %v, want no error", err)
	}
	defer pool.Close()

	addr, stop := run(t, &config.Config{}, connectors.Deps{
		Pool:    pool,
		Sink:    &connector.Recorder{},
		Cursors: connector.NewMemoryCursors(),
	})
	code, body := get(t, addr, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz against an unmigrated database = %d, want 503: %s", code, body)
	}
	if !strings.Contains(body, "migrate up") {
		t.Errorf("the readiness body does not say what to run: %s", body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

// swapDatabaseName points a connection URL at another database on the same
// server.
func swapDatabaseName(url, name string) string {
	base, query, hasQuery := strings.Cut(url, "?")
	swapped := base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		return swapped + "?" + query
	}
	return swapped
}

// artifactsIn is what L0 holds for one source, sorted, which is what a test
// compares.
func artifactsIn(t *testing.T, store *l0.Store, source string) []string {
	t.Helper()
	events, err := store.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: source}})
	if err != nil {
		t.Fatalf("List() = %v, want no error", err)
	}
	out := []string{}
	for _, ev := range events {
		out = append(out, ev.Payload.Artifact)
	}
	slices.Sort(out)
	return out
}
