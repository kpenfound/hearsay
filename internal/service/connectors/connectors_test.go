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

func quick() connector.Cadence {
	return connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}
}

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

type hosted struct {
	src     connector.SourceConfig
	fake    *connector.Fake
	rec     *connector.Recorder
	cursors *connector.MemoryCursors
	addr    string
	stop    func() error
}

func hostFake(t *testing.T) *hosted {
	t.Helper()
	src := source("fake-eng")
	fake := connector.NewFake(src)
	fake.Queue = []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "polled")}
	fake.Pages = [][]connector.Event{{fake.NewEvent(connector.KindMessage, "h1", "history")}}
	h := &hosted{src: src, fake: fake, rec: &connector.Recorder{}, cursors: connector.NewMemoryCursors()}
	cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}
	h.addr, h.stop = run(t, cfg, connectors.Deps{
		Registry: fakeRegistry(t, map[string]connector.Connector{src.ID: fake}),
		Sink:     h.rec,
		Cursors:  h.cursors,
	})
	return h
}

func (h *hosted) post(t *testing.T, ev connector.Event) int {
	t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshalling the event: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+h.addr+connector.HookPath(h.src.ID), strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST the hook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestTheServiceAnswersHealthz(t *testing.T) {
	h := hostFake(t)
	code, body := get(t, h.addr, "/healthz")
	if code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Errorf("GET /healthz = %d %q, want 200 and ok", code, body)
	}
	if err := h.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

func TestReadyzReportsTheHostedSource(t *testing.T) {
	h := hostFake(t)
	waitFor(t, "the backfill to finish", func() bool {
		_, body := get(t, h.addr, "/readyz")
		return strings.Contains(body, `"backfill_done": true`)
	})
	code, body := get(t, h.addr, "/readyz")
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
	if len(ready.Sources) != 1 || ready.Sources[0].Source != h.src.ID {
		t.Fatalf("readiness lists %+v, want the one configured source", ready.Sources)
	}
	if !ready.Sources[0].BackfillDone || ready.Sources[0].Backfilled != 1 {
		t.Errorf("readiness says backfill done=%v after %d events, want done after one",
			ready.Sources[0].BackfillDone, ready.Sources[0].Backfilled)
	}
	if err := h.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

func TestTheServiceAcceptsAWebhookDelivery(t *testing.T) {
	h := hostFake(t)
	if code := h.post(t, h.fake.NewEvent(connector.KindMessage, "p1", "pushed")); code != http.StatusAccepted {
		t.Errorf("POST %s = %d, want 202", connector.HookPath(h.src.ID), code)
	}
	if err := h.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

func TestThePolledBackfilledAndPushedEventsReachTheSink(t *testing.T) {
	h := hostFake(t)
	waitFor(t, "the polled and backfilled events", func() bool { return len(h.rec.Events()) >= 2 })
	h.post(t, h.fake.NewEvent(connector.KindMessage, "p1", "pushed"))
	if err := h.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	got := []string{}
	for _, ev := range h.rec.Events() {
		got = append(got, ev.Payload.Artifact)
	}
	slices.Sort(got)
	if want := []string{"h1", "m1", "p1"}; !slices.Equal(slices.Compact(got), want) {
		t.Errorf("the sink received %v, want the polled, backfilled and pushed events %v", got, want)
	}
}

func TestTheBackfillSavesItsPosition(t *testing.T) {
	h := hostFake(t)
	waitFor(t, "the backfill to save a position", func() bool { return len(h.cursors.Saves(h.src.ID)) > 0 })
	if err := h.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

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

	if code, _ := get(t, addr, "/healthz"); code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", code)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

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

type blockingPusher struct {
	fake     *connector.Fake
	started  chan struct{}
	release  chan struct{}
	inFlight atomic.Bool

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

	stopped := make(chan error, 1)
	go func() { stopped <- stop() }()

	waitFor(t, "the listener to close", func() bool {
		dialer := net.Dialer{Timeout: time.Second}
		conn, err := dialer.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			return true
		}
		_ = conn.Close()
		return false
	})
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

type resyncing struct {
	*connector.Fake
	calls atomic.Int64
}

func (r *resyncing) Public(context.Context, string) (bool, error) { return false, nil }

func (r *resyncing) Resync(context.Context, connector.Sink, string, connector.Cursor) (connector.BackfillResult, error) {
	r.calls.Add(1)
	return connector.BackfillResult{Done: true}, nil
}

type exposed struct{}

func (exposed) Exposed(context.Context, string) ([]connector.Exposure, error) {
	return []connector.Exposure{{Container: "C123", LastPublic: time.Now()}}, nil
}

func TestTheServiceGivesTheRuntimeItsResyncStores(t *testing.T) {
	src := source("fake-resync")
	conn := &resyncing{Fake: connector.NewFake(src)}
	resyncs := connector.NewMemoryResyncs()
	cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}
	_, stop := run(t, cfg, connectors.Deps{
		Registry: fakeRegistry(t, map[string]connector.Connector{src.ID: conn}),
		Sink:     &connector.Recorder{},
		Cursors:  connector.NewMemoryCursors(),
		Resyncs:  resyncs,
		Exposure: exposed{},
	})
	waitFor(t, "the re-sync the check owed", func() bool {
		r := resyncs.Get(src.ID, "C123")
		return r.Generation > 0 && !r.Owed
	})
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
}

func TestTheRuntimeNeverResyncsWithoutAResyncStore(t *testing.T) {
	src := source("fake-resync")
	bare := &resyncing{Fake: connector.NewFake(src)}
	bare.Queue = []connector.Event{bare.NewEvent(connector.KindMessage, "m1", "polled")}
	rec := &connector.Recorder{}
	cfg := &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}
	_, stop := run(t, cfg, connectors.Deps{
		Registry: fakeRegistry(t, map[string]connector.Connector{src.ID: bare}),
		Sink:     rec,
		Cursors:  connector.NewMemoryCursors(),
	})
	waitFor(t, "the connector to be polled", func() bool { return len(rec.Events()) > 0 })
	if err := stop(); err != nil {
		t.Fatalf("Run() with no re-sync store = %v", err)
	}
	if n := bare.calls.Load(); n != 0 {
		t.Errorf("with no re-sync store the runtime re-synced %d times, want none", n)
	}
}
