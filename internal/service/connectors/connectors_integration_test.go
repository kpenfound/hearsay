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

func newSourceID(t *testing.T) string {
	t.Helper()
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "conn"
}

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

	waitFor(t, "the backfill to store its final position", func() bool {
		state, err := cursors.Load(t.Context(), id)
		return err == nil && state.Done && state.Events == 2
	})
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

	second := &pager{Fake: connector.NewFake(src)}
	second.Pages = fake.Pages
	addr, stop = run(t, cfg, connectors.Deps{
		Pool:     pool,
		Registry: fakeRegistry(t, map[string]connector.Connector{id: second}),
	})
	waitFor(t, "the restarted process to read the stored position", func() bool {
		_, body := get(t, addr, "/readyz")
		return strings.Contains(body, `"backfill_done": true`)
	})
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

func TestReadyzFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	newPool(t)

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

	for _, secret := range []string{"127.0.0.1", "hearsay@", "closed"} {
		if strings.Contains(body, secret) {
			t.Errorf("the readiness body carries %q, which is the operator's to read in the log: %s", secret, body)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

func TestReadyzFailsWhenTheSchemaIsBehindTheBinary(t *testing.T) {
	newPool(t)

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

func swapDatabaseName(url, name string) string {
	base, query, hasQuery := strings.Cut(url, "?")
	swapped := base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		return swapped + "?" + query
	}
	return swapped
}

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

func TestTheServiceRecordsAndAppliesACommand(t *testing.T) {
	pool := newPool(t)
	store := l0.New(pool)
	tests := []struct {
		name     string
		readOnly bool
		status   int
		want     connector.CommandOutcome
		recorded []string
	}{
		{name: "a source that takes commands", status: http.StatusOK, want: connector.CommandUnmapped, recorded: []string{"interaction:1"}},
		{name: "a read-only source", readOnly: true, status: http.StatusNoContent, recorded: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := newSourceID(t)
			src := connector.SourceConfig{ID: id, Type: connector.FakeType, Containers: []string{"C123"}, Refresh: time.Hour, ReadOnly: tt.readOnly}
			cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}
			fake := connector.NewFake(src)
			addr, stop := run(t, cfg, connectors.Deps{Pool: pool, Registry: fakeRegistry(t, map[string]connector.Connector{id: fake})})
			defer func() {
				if err := stop(); err != nil {
					t.Errorf("Run() = %v, want nil", err)
				}
			}()

			body, err := json.Marshal(connector.Command{Event: fake.NewEvent(connector.KindCommand, "interaction:1", "/hearsay pin"), Verb: connector.CommandPin, Target: "C123/1"})
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+connector.HookPath(id)+"?command", strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST the command: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.status {
				t.Fatalf("the command was answered %d, want %d", resp.StatusCode, tt.status)
			}
			if tt.status == http.StatusOK {
				var got connector.CommandResult
				if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				if got.Outcome != tt.want {
					t.Errorf("the result is %+v, want %s", got, tt.want)
				}
			}
			if got := artifactsIn(t, store, id); !slices.Equal(got, tt.recorded) {
				t.Errorf("L0 holds %v, want %v", got, tt.recorded)
			}
			if len(tt.recorded) == 0 {
				return
			}
			ev, err := store.Get(t.Context(), connector.EventID(id, "interaction:1"))
			if err != nil || ev.Kind != connector.KindCommand {
				t.Errorf("the recorded event is %+v, %v; want a command", ev, err)
			}
		})
	}
}
