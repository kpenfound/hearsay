package connectors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/service/connectors"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

func source(id string) connector.SourceConfig {
	return connector.SourceConfig{ID: id, Type: connector.FakeType, Containers: []string{"C123"}, Refresh: time.Millisecond}
}

// quick is the cadence the service's tests run on: the runtime's own floor is a
// deployment's thirty seconds.
func quick() connector.Cadence {
	return connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}
}

// fakeRegistry hands out the connectors a test built, by source id.
func fakeRegistry(t *testing.T, conns map[string]connector.Connector) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	err := reg.Register(connector.FakeType, func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
		c, ok := conns[src.ID]
		if !ok {
			return nil, errors.New("no connector for " + src.ID)
		}
		return c, nil
	})
	if err != nil {
		t.Fatalf("Register() = %v, want no error", err)
	}
	return reg
}

// run starts the service on a listener of its own and returns its address and a
// function that stops it.
func run(t *testing.T, cfg *config.Config, deps connectors.Deps) (string, func() error) {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	deps.Listener = listener
	if deps.Cadence == (connector.Cadence{}) {
		deps.Cadence = quick()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- connectors.Run(ctx, cfg, deps) }()
	return listener.Addr().String(), func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the service did not stop within 10s of cancellation")
			return nil
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 10s waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func get(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body of %s: %v", path, err)
	}
	return resp.StatusCode, string(body)
}

// The service hosts what configuration names, polls it, and serves the two
// endpoints every service has (ADR-0008) plus the push connectors' handlers.
func TestTheServiceHostsPollsAndServes(t *testing.T) {
	src := source("fake-eng")
	fake := connector.NewFake(src)
	fake.Queue = []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "polled")}
	fake.Pages = [][]connector.Event{{fake.NewEvent(connector.KindMessage, "h1", "history")}}
	rec := &connector.Recorder{}
	cursors := connector.NewMemoryCursors()

	cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}
	addr, stop := run(t, cfg, connectors.Deps{
		Registry: fakeRegistry(t, map[string]connector.Connector{src.ID: fake}),
		Sink:     rec,
		Cursors:  cursors,
	})

	waitFor(t, "the polled and backfilled events", func() bool { return len(rec.Events()) >= 2 })

	t.Run("healthz", func(t *testing.T) {
		code, body := get(t, addr, "/healthz")
		if code != http.StatusOK || !strings.Contains(body, "ok") {
			t.Errorf("GET /healthz = %d %q, want 200 and ok", code, body)
		}
	})

	t.Run("readyz reports the source", func(t *testing.T) {
		waitFor(t, "the backfill to finish", func() bool {
			_, body := get(t, addr, "/readyz")
			return strings.Contains(body, `"backfill_done": true`)
		})
		code, body := get(t, addr, "/readyz")
		if code != http.StatusOK {
			t.Errorf("GET /readyz = %d, want 200: %s", code, body)
		}
		var ready connectors.Readiness
		if err := json.Unmarshal([]byte(body), &ready); err != nil {
			t.Fatalf("readiness is not JSON: %v (%s)", err, body)
		}
		if ready.Status != connector.HealthOK {
			t.Errorf("readiness status = %q, want ok: %s", ready.Status, body)
		}
		if len(ready.Sources) != 1 || ready.Sources[0].Source != src.ID {
			t.Fatalf("readiness lists %+v, want the one configured source", ready.Sources)
		}
		if !ready.Sources[0].BackfillDone || ready.Sources[0].Backfilled != 1 {
			t.Errorf("readiness says backfill done=%v after %d events, want done after one",
				ready.Sources[0].BackfillDone, ready.Sources[0].Backfilled)
		}
	})

	t.Run("a webhook delivery", func(t *testing.T) {
		body, err := json.Marshal(fake.NewEvent(connector.KindMessage, "p1", "pushed"))
		if err != nil {
			t.Fatalf("marshalling the event: %v", err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			"http://"+addr+connector.HookPath(src.ID), strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST the hook: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("POST %s = %d, want 202", connector.HookPath(src.ID), resp.StatusCode)
		}
	})

	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	got := []string{}
	for _, ev := range rec.Events() {
		got = append(got, ev.Payload.Artifact)
	}
	slices.Sort(got)
	if want := []string{"h1", "m1", "p1"}; !slices.Equal(slices.Compact(got), want) {
		t.Errorf("the sink received %v, want the polled, backfilled and pushed events %v", got, want)
	}
	if len(cursors.Saves(src.ID)) == 0 {
		t.Error("the backfill saved no position")
	}
}

// A source that has failed is a process that is not ready: a load balancer, or
// an operator, must be able to tell.
func TestReadyzFailsWhenASourceHasFailed(t *testing.T) {
	src := source("fake-eng")
	fake := connector.NewFake(src)
	fake.Status = connector.HealthFailed
	cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}

	addr, stop := run(t, cfg, connectors.Deps{
		Registry: fakeRegistry(t, map[string]connector.Connector{src.ID: fake}),
		Sink:     &connector.Recorder{},
		Cursors:  connector.NewMemoryCursors(),
	})
	code, body := get(t, addr, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz with a failed source = %d, want 503: %s", code, body)
	}
	// Up is not ready: the process is running, and that is all /healthz says.
	if code, _ := get(t, addr, "/healthz"); code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", code)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

// A process with no configuration hosts nothing, and says so by serving rather
// than by exiting: that is what every other service does without one (ADR-0009).
func TestNoConfigurationHostsNothing(t *testing.T) {
	addr, stop := run(t, &config.Config{}, connectors.Deps{
		Sink:    &connector.Recorder{},
		Cursors: connector.NewMemoryCursors(),
	})
	code, body := get(t, addr, "/readyz")
	if code != http.StatusOK {
		t.Errorf("GET /readyz with nothing configured = %d, want 200: %s", code, body)
	}
	if !strings.Contains(body, `"sources": []`) {
		t.Errorf("readiness = %s, want no sources", body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

// Writing L0 is what a connector does, and a backfill that cannot store its
// position walks a source's history again on every restart. A process with
// neither a database nor something in its place cannot do the job.
func TestTheServiceNeedsSomewhereToWriteAndSomewhereToResumeFrom(t *testing.T) {
	tests := []struct {
		name string
		deps connectors.Deps
	}{
		{name: "nothing at all", deps: connectors.Deps{}},
		{name: "a sink but nowhere to keep a cursor", deps: connectors.Deps{Sink: &connector.Recorder{}}},
		{name: "a cursor store but nowhere to write", deps: connectors.Deps{Cursors: connector.NewMemoryCursors()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := connectors.Run(t.Context(), &config.Config{}, tt.deps)
			if err == nil || !strings.Contains(err.Error(), "database") {
				t.Fatalf("Run() = %v, want an error about the database", err)
			}
		})
	}
}

// Every line this service writes says which connectors the process is hosting
// and, where it is about one, which source — and each of those is one field
// with one value. `service.Stub`'s doc comment names the failure this pins:
// `telemetry.With` appends, so two writers reaching for the same key produce a
// line with the key twice, and `encoding/json` keeps the last.
func TestALineNamesTheProcessAndItsSourceOnce(t *testing.T) {
	src := source("fake-eng")
	fake := connector.NewFake(src)
	fake.Queue = []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "polled")}
	rec := &connector.Recorder{}

	var out syncBuffer
	ctx := telemetry.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&out, nil)))
	ctx, cancel := context.WithCancel(ctx)

	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}
	done := make(chan error, 1)
	go func() {
		done <- connectors.Run(ctx, cfg, connectors.Deps{
			Registry: fakeRegistry(t, map[string]connector.Connector{src.ID: fake}),
			Sink:     rec,
			Cursors:  connector.NewMemoryCursors(),
			Listener: listener,
			Cadence:  quick(),
		})
	}()
	waitFor(t, "the connector to be polled", func() bool { return len(rec.Events()) >= 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return within 10s of cancellation")
	}

	lines := 0
	sourceLines := 0
	for line := range strings.Lines(strings.TrimSpace(out.String())) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines++
		// Count the occurrences rather than trusting the decoded value:
		// json.Unmarshal keeps the last of a repeated key, which is the bug.
		for _, field := range []string{"hosting", "source"} {
			if n := strings.Count(line, `"`+field+`":`); n > 1 {
				t.Errorf("a log line carries the %s field %d times, want at most once: %s", field, n, line)
			}
		}
		if n := strings.Count(line, `"hosting":`); n != 1 {
			t.Errorf("a log line does not say which connectors the process hosts: %s", line)
		}
		if strings.Contains(line, `"source":"`+src.ID+`"`) {
			sourceLines++
		}
	}
	if lines == 0 {
		t.Fatal("the service logged nothing")
	}
	if sourceLines == 0 {
		t.Errorf("no line names the source it is about:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"hosting":"all"`) {
		t.Errorf("no line says the process hosts every configured connector:\n%s", out.String())
	}
}

// blockingPusher is a push connector whose handler a test can hold open, so
// that shutdown can be observed while a delivery is in flight.
type blockingPusher struct {
	fake     *connector.Fake
	started  chan struct{}
	release  chan struct{}
	inFlight atomic.Bool
	// closedInFlight records what Close saw: true means a connector was closed
	// while one of its own deliveries was still being served.
	closedInFlight atomic.Bool
}

func (b *blockingPusher) Describe() connector.Descriptor              { return b.fake.Describe() }
func (b *blockingPusher) Health(ctx context.Context) connector.Health { return b.fake.Health(ctx) }

func (b *blockingPusher) Close(ctx context.Context) error {
	b.closedInFlight.Store(b.inFlight.Load())
	return b.fake.Close(ctx)
}

func (b *blockingPusher) Handler(sink connector.Sink) http.Handler {
	inner := b.fake.Handler(sink)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.inFlight.Store(true)
		defer b.inFlight.Store(false)
		close(b.started)
		<-b.release
		inner.ServeHTTP(w, r)
	})
}

// A connector is closed after the deliveries it is serving have finished. The
// two shutdowns are ordered, so a `Close` that lets go of what a handler is
// using cannot pull it out from under one.
func TestAConnectorIsClosedAfterItsDeliveriesFinish(t *testing.T) {
	src := source("fake-eng")
	pusher := &blockingPusher{
		fake:    connector.NewFake(src),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	rec := &connector.Recorder{}
	cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}
	addr, stop := run(t, cfg, connectors.Deps{
		Registry: fakeRegistry(t, map[string]connector.Connector{src.ID: pusher}),
		Sink:     rec,
		Cursors:  connector.NewMemoryCursors(),
	})

	body, err := json.Marshal(pusher.fake.NewEvent(connector.KindMessage, "p1", "delivered"))
	if err != nil {
		t.Fatalf("marshalling the event: %v", err)
	}
	delivered := make(chan int, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodPost,
			"http://"+addr+connector.HookPath(src.ID), strings.NewReader(string(body)))
		if err != nil {
			delivered <- 0
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			delivered <- 0
			return
		}
		defer func() { _ = resp.Body.Close() }()
		delivered <- resp.StatusCode
	}()
	<-pusher.started

	// Stop the service while the delivery is inside the handler, then let it
	// finish: the shutdown has to wait for it.
	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()
	time.Sleep(50 * time.Millisecond)
	close(pusher.release)

	if code := <-delivered; code != http.StatusAccepted {
		t.Errorf("the delivery in flight when the process stopped = %d, want 202", code)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if pusher.closedInFlight.Load() {
		t.Error("the connector was closed while it was still serving a delivery")
	}
	if got, want := len(rec.Events()), 1; got != want {
		t.Errorf("the sink received %d events, want the one that was in flight", got)
	}
}

// syncBuffer is a bytes.Buffer a test can read while a goroutine writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A `--source` selection is which of the configured connectors this process
// hosts (ADR-0003).
func TestSelect(t *testing.T) {
	configured := []connector.SourceConfig{source("github"), source("discord"), source("drive")}
	tests := []struct {
		name     string
		selected []string
		want     []string
		wantErr  string
	}{
		{name: "no selection is all of them", want: []string{"github", "discord", "drive"}},
		{name: "one of them", selected: []string{"discord"}, want: []string{"discord"}},
		{name: "several, in the order they were given", selected: []string{"drive", "github"}, want: []string{"drive", "github"}},
		{name: "the same one twice is hosted once", selected: []string{"drive", "drive"}, want: []string{"drive"}},
		{name: "a name nothing is configured under", selected: []string{"githbu"}, wantErr: "githbu"},
		{name: "one good name and one typo", selected: []string{"github", "githbu"}, wantErr: "githbu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := connectors.Select(configured, tt.selected)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Select(%v) = %v, want an error naming %q", tt.selected, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select(%v) = %v, want no error", tt.selected, err)
			}
			ids := []string{}
			for _, src := range got {
				ids = append(ids, src.ID)
			}
			if !slices.Equal(ids, tt.want) {
				t.Errorf("Select(%v) = %v, want %v", tt.selected, ids, tt.want)
			}
		})
	}
}

// The `hosting` log field says which connectors a process is hosting.
func TestSelection(t *testing.T) {
	tests := []struct {
		name    string
		sources []string
		want    string
	}{
		{name: "none is all of them", want: "all"},
		{name: "one", sources: []string{"github"}, want: "github"},
		{name: "several", sources: []string{"github", "discord"}, want: "github,discord"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectors.Selection(tt.sources); got != tt.want {
				t.Errorf("Selection(%v) = %q, want %q", tt.sources, got, tt.want)
			}
		})
	}
}
