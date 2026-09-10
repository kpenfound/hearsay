package connector_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// The store the runtime keeps backfill positions in, and the one a test uses,
// are the same interface.
var _ connector.CursorStore = (*connector.MemoryCursors)(nil)

// runtimeSource is a source with a cadence a test can wait for: the runtime's
// own floor is thirty seconds, which is a deployment's number and not a test's.
func runtimeSource(id string) connector.SourceConfig {
	return connector.SourceConfig{ID: id, Type: connector.FakeType, Containers: []string{"C123"}, Refresh: time.Millisecond}
}

// quick is the cadence every runtime test runs on.
func quick() connector.Cadence {
	return connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}
}

// registryOf returns a registry whose factory hands out the connectors a test
// built, one per source id.
func registryOf(t *testing.T, conns map[string]connector.Connector) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	if err := reg.Register(connector.FakeType, func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
		c, ok := conns[src.ID]
		if !ok {
			return nil, errors.New("no connector for " + src.ID)
		}
		return c, nil
	}); err != nil {
		t.Fatalf("Register() = %v, want no error", err)
	}
	return reg
}

// start runs a runtime in the background and returns a function that stops it
// and reports what Run returned. Every test that starts one defers it, so a
// runtime that will not shut down fails the test rather than leaking into the
// next one.
func start(t *testing.T, opts connector.RuntimeOptions) (*connector.Runtime, func() error) {
	t.Helper()
	if opts.Cadence == (connector.Cadence{}) {
		opts.Cadence = quick()
	}
	runtime, err := connector.NewRuntime(t.Context(), opts)
	if err != nil {
		t.Fatalf("NewRuntime() = %v, want no error", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	return runtime, func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the runtime did not stop within 10s of cancellation")
			return nil
		}
	}
}

// waitFor blocks until cond holds, and fails the test if it never does: a test
// that waits without a bound reads as passing when the thing it waits for has
// stopped happening.
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

// poller counts polls, and reports whether two of them ever overlapped.
type poller struct {
	*connector.Fake
	hold     time.Duration
	polls    atomic.Int64
	inPoll   atomic.Int64
	overlap  atomic.Bool
	closes   atomic.Int64
	failures int64 // fail this many polls before the first success
}

func (p *poller) Poll(ctx context.Context, sink connector.Sink) error {
	if p.inPoll.Add(1) > 1 {
		p.overlap.Store(true)
	}
	defer p.inPoll.Add(-1)
	if p.hold > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(p.hold):
		}
	}
	if n := p.polls.Add(1); n <= p.failures {
		return errors.New("the source is unreachable")
	}
	return p.Fake.Poll(ctx, sink)
}

func (p *poller) Close(ctx context.Context) error {
	p.closes.Add(1)
	return p.Fake.Close(ctx)
}

// Two ticks of one connector's Poll never run at the same time, which is what
// lets a poller keep its position in memory without locking
// (docs/connector-contract.md).
func TestPollIsNeverConcurrentWithItself(t *testing.T) {
	src := runtimeSource("fake-eng")
	fake := connector.NewFake(src)
	p := &poller{Fake: fake, hold: 5 * time.Millisecond}
	rec := &connector.Recorder{}

	_, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: p}),
		Sink:     rec,
	})
	// Several ticks, each of which takes five times the interval: a runtime
	// that started a goroutine per tick would have them piled up by now.
	waitFor(t, "five polls", func() bool { return p.polls.Load() >= 5 })
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if p.overlap.Load() {
		t.Error("two polls of one connector ran at the same time")
	}
	if got := p.closes.Load(); got != 1 {
		t.Errorf("the connector was closed %d times, want once", got)
	}
	// And the runtime closed it: a closed fake refuses to ingest.
	if err := p.Fake.Poll(t.Context(), rec); !errors.Is(err, connector.ErrClosed) {
		t.Errorf("Poll() after the runtime stopped = %v, want ErrClosed", err)
	}
}

// A poll that fails is retried on the next tick rather than ending the loop,
// and the failures are visible in health until one works.
func TestAPollThatFailsIsRetried(t *testing.T) {
	src := runtimeSource("fake-eng")
	fake := connector.NewFake(src)
	p := &poller{Fake: fake, failures: 2}
	fake.Queue = []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "after two failures")}
	rec := &connector.Recorder{}

	runtime, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: p}),
		Sink:     rec,
	})
	waitFor(t, "the failures to be reported", func() bool {
		return runtime.Health(t.Context()).Sources[0].PollFailures >= 2
	})
	waitFor(t, "the event that follows them", func() bool { return len(rec.Events()) == 1 })
	waitFor(t, "the failure count to clear", func() bool {
		return runtime.Health(t.Context()).Sources[0].PollFailures == 0
	})
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
}

// An event from a container config does not list is dropped before it reaches
// the sink, and counted, because a container nobody put in config is not an
// error anywhere (docs/design.md#access-control, control point 1).
func TestAnEventOutsideTheAllowlistIsDroppedAndCounted(t *testing.T) {
	src := runtimeSource("fake-eng")
	fake := connector.NewFake(src)
	allowed := fake.NewEvent(connector.KindMessage, "m1", "in a listed container")
	outside := fake.NewEvent(connector.KindMessage, "m2", "in a container nobody listed")
	outside.Payload.Container.NativeID = "C999"
	fake.Queue = []connector.Event{allowed, outside}
	rec := &connector.Recorder{}

	runtime, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: fake}),
		Sink:     rec,
	})
	waitFor(t, "the allowed event", func() bool { return len(rec.Events()) >= 1 })
	waitFor(t, "the drop to be counted", func() bool {
		return runtime.Health(t.Context()).Sources[0].Dropped >= 1
	})
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	for _, ev := range rec.Events() {
		if ev.Payload.Container.NativeID != "C123" {
			t.Errorf("an event from container %q reached the sink; the allowlist is %v", ev.Payload.Container.NativeID, src.Containers)
		}
		if ev.Payload.Artifact == "m2" {
			t.Error("the event from the unlisted container reached the sink")
		}
	}
	if got := runtime.Health(t.Context()).Sources[0].Dropped; got < 1 {
		t.Errorf("Dropped = %d, want at least one", got)
	}
}

// pager is a backfiller that records the cursor it was called with, and can be
// held between calls so that a test knows exactly how far a backfill got.
type pager struct {
	*connector.Fake
	tokens chan struct{} // one token per call; nil is no waiting

	mu   sync.Mutex
	from []connector.Cursor
}

func (p *pager) Backfill(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.BackfillResult, error) {
	p.mu.Lock()
	p.from = append(p.from, from)
	p.mu.Unlock()
	if p.tokens != nil {
		select {
		case <-p.tokens:
		case <-ctx.Done():
			return connector.BackfillResult{}, ctx.Err()
		}
	}
	return p.Fake.Backfill(ctx, sink, from)
}

func (p *pager) cursors() []connector.Cursor {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.from)
}

// pages builds a fake with three pages of history, one event each.
func pages(src connector.SourceConfig) *connector.Fake {
	fake := connector.NewFake(src)
	for _, id := range []string{"h1", "h2", "h3"} {
		fake.Pages = append(fake.Pages, []connector.Event{fake.NewEvent(connector.KindMessage, id, "history "+id)})
	}
	return fake
}

// A backfill interrupted by a restart resumes from the position that was
// stored, not from the beginning of history: the restarted process must not
// walk a source's whole history again.
func TestBackfillResumesFromThePersistedCursor(t *testing.T) {
	src := runtimeSource("fake-eng")
	cursors := connector.NewMemoryCursors()

	// The first process gets one page done, and is stopped while the second
	// call is waiting for a token it never gets.
	first := &pager{Fake: pages(src), tokens: make(chan struct{}, 1)}
	firstRec := &connector.Recorder{}
	_, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: first}),
		Sink:     firstRec,
		Cursors:  cursors,
	})
	first.tokens <- struct{}{}
	waitFor(t, "the first page to be persisted", func() bool { return len(cursors.Saves(src.ID)) == 1 })
	if err := stop(); err != nil {
		t.Fatalf("Run(first) = %v, want nil", err)
	}

	saved := cursors.Saves(src.ID)[0]
	if saved.Cursor == "" || saved.Done {
		t.Fatalf("after one page the stored position is %+v, want a cursor and not done", saved)
	}

	// The second process resumes: it asks for the stored cursor first, and
	// nothing from the page the first process already walked reaches its sink.
	second := &pager{Fake: pages(src)}
	secondRec := &connector.Recorder{}
	runtime, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: second}),
		Sink:     secondRec,
		Cursors:  cursors,
	})
	waitFor(t, "the rest of history", func() bool {
		return runtime.Health(t.Context()).Sources[0].BackfillDone
	})
	if err := stop(); err != nil {
		t.Fatalf("Run(second) = %v, want nil", err)
	}

	if got := second.cursors(); len(got) == 0 || got[0] != saved.Cursor {
		t.Errorf("the restarted backfill started at %v, want it to start at the stored cursor %q", got, saved.Cursor)
	}
	for _, ev := range secondRec.Events() {
		if ev.Payload.Artifact == "h1" {
			t.Errorf("the restarted backfill re-emitted %q, which the first process had already walked past", ev.Payload.Artifact)
		}
	}
	if got, want := artifacts(secondRec), []string{"h2", "h3"}; !slices.Equal(got, want) {
		t.Errorf("the restarted backfill emitted %v, want %v", got, want)
	}

	// And history is exhausted, so a third process does not walk it again.
	final, err := cursors.Load(t.Context(), src.ID)
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if !final.Done {
		t.Errorf("the stored position is %+v, want done", final)
	}
	if final.Events != 3 {
		t.Errorf("the stored position counts %d events, want the three pages' worth", final.Events)
	}
}

// The position is stored after every call rather than at the end, which is what
// makes an interrupted backfill resume rather than start again.
func TestBackfillPersistsAfterEveryCall(t *testing.T) {
	src := runtimeSource("fake-eng")
	cursors := connector.NewMemoryCursors()
	fake := pages(src)
	rec := &connector.Recorder{}

	runtime, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: fake}),
		Sink:     rec,
		Cursors:  cursors,
	})
	waitFor(t, "history", func() bool { return runtime.Health(t.Context()).Sources[0].BackfillDone })
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	saves := cursors.Saves(src.ID)
	if len(saves) != len(fake.Pages) {
		t.Fatalf("the backfill saved %d positions for %d pages: %+v", len(saves), len(fake.Pages), saves)
	}
	for i, save := range saves {
		if want := i == len(saves)-1; save.Done != want {
			t.Errorf("save %d is done=%v, want %v", i, save.Done, want)
		}
		if want := int64(i + 1); save.Events != want {
			t.Errorf("save %d counts %d events, want %d", i, save.Events, want)
		}
	}
	// The last call reports Done, and its Next is meaningless: the position
	// stays where the last call that meant one left it.
	if got, want := saves[len(saves)-1].Cursor, saves[len(saves)-2].Cursor; got != want {
		t.Errorf("the position after the last page is %q, want the one before it (%q): Next is ignored when Done is set", got, want)
	}
}

// stuck is a backfiller that hands back the cursor it was given and emits
// nothing: the contract says one call does a bounded amount of work and reports
// Done when there is no more, so this is a connector to fix.
type stuck struct {
	*connector.Fake
	calls atomic.Int64
}

func (s *stuck) Backfill(_ context.Context, _ connector.Sink, from connector.Cursor) (connector.BackfillResult, error) {
	s.calls.Add(1)
	return connector.BackfillResult{Next: from}, nil
}

// A backfill that makes no progress is a failure and not the end of history:
// recording a walk that never happened would lose the source's history for
// good, and calling it again immediately would be a hot loop against the
// source's API.
func TestABackfillThatMakesNoProgressIsNotProgress(t *testing.T) {
	src := runtimeSource("fake-eng")
	cursors := connector.NewMemoryCursors()
	s := &stuck{Fake: connector.NewFake(src)}

	_, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: s}),
		Sink:     &connector.Recorder{},
		Cursors:  cursors,
	})
	waitFor(t, "the backfill to be tried", func() bool { return s.calls.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if got := cursors.Saves(src.ID); len(got) != 0 {
		t.Errorf("a backfill that emitted nothing and did not move stored %d positions: %+v", len(got), got)
	}
	if got, err := cursors.Load(t.Context(), src.ID); err != nil || got.Done {
		t.Errorf("Load() = %+v, %v, want a source whose history is not recorded as walked", got, err)
	}
}

// A source whose history is already walked is not walked again on the next
// start.
func TestADoneBackfillIsNotRestarted(t *testing.T) {
	src := runtimeSource("fake-eng")
	cursors := connector.NewMemoryCursors()
	cursors.Set(src.ID, connector.BackfillState{Cursor: "3", Done: true, Events: 3})
	p := &pager{Fake: pages(src)}
	rec := &connector.Recorder{}

	_, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: p}),
		Sink:     rec,
		Cursors:  cursors,
	})
	// Long enough for many ticks of a one-millisecond cadence.
	time.Sleep(50 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if got := p.cursors(); len(got) != 0 {
		t.Errorf("Backfill was called %d times for a source whose history is done: %v", len(got), got)
	}
	if got := cursors.Saves(src.ID); len(got) != 0 {
		t.Errorf("the position was written %d times for a source whose history is done: %+v", len(got), got)
	}
}

// With nowhere to keep a position, the runtime polls and does not backfill:
// walking the whole of a source's history on every restart is worse than not
// walking it.
func TestBackfillNeedsACursorStore(t *testing.T) {
	src := runtimeSource("fake-eng")
	p := &pager{Fake: pages(src)}
	rec := &connector.Recorder{}

	_, stop := start(t, connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: p}),
		Sink:     rec,
	})
	waitFor(t, "a poll", func() bool { return true })
	time.Sleep(20 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if got := p.cursors(); len(got) != 0 {
		t.Errorf("Backfill was called %d times with no cursor store: %v", len(got), got)
	}
}

// A push connector's handler is mounted under the path the runtime owns, and
// what it emits goes through the same gate a poll does.
func TestPushHandlersAreMounted(t *testing.T) {
	src := runtimeSource("fake-eng")
	fake := connector.NewFake(src)
	rec := &connector.Recorder{}
	runtime, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registryOf(t, map[string]connector.Connector{src.ID: fake}),
		Sink:     rec,
		Cadence:  quick(),
	})
	if err != nil {
		t.Fatalf("NewRuntime() = %v, want no error", err)
	}
	defer func() {
		if err := runtime.Close(t.Context()); err != nil {
			t.Errorf("Close() = %v, want no error", err)
		}
	}()

	body, err := json.Marshal(fake.NewEvent(connector.KindMessage, "m1", "delivered"))
	if err != nil {
		t.Fatalf("marshalling the event: %v", err)
	}
	tests := []struct {
		name string
		path string
		want int
	}{
		{name: "the source's own path", path: connector.HookPath(src.ID), want: http.StatusAccepted},
		{name: "a source nobody configured", path: connector.HookPath("nobody"), want: http.StatusNotFound},
		{name: "the prefix itself", path: connector.HookPrefix, want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, strings.NewReader(string(body)))
			w := httptest.NewRecorder()
			runtime.Handler().ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("POST %s = %d, want %d", tt.path, w.Code, tt.want)
			}
		})
	}
	if got, want := artifacts(rec), []string{"m1"}; !slices.Equal(got, want) {
		t.Errorf("the delivery emitted %v, want %v", got, want)
	}
}

// Health is each connector's own account of itself, and the worst of them is
// the runtime's.
func TestHealthAggregatesEveryConnector(t *testing.T) {
	tests := []struct {
		name     string
		statuses []connector.HealthStatus
		want     connector.HealthStatus
	}{
		{name: "no sources at all", want: connector.HealthOK},
		{name: "every source working", statuses: []connector.HealthStatus{connector.HealthOK, connector.HealthOK}, want: connector.HealthOK},
		{name: "one source behind", statuses: []connector.HealthStatus{connector.HealthOK, connector.HealthDegraded}, want: connector.HealthDegraded},
		{name: "one source failed", statuses: []connector.HealthStatus{connector.HealthDegraded, connector.HealthFailed}, want: connector.HealthFailed},
		{name: "a status nobody defined", statuses: []connector.HealthStatus{"weather"}, want: connector.HealthFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sources []connector.SourceConfig
			conns := map[string]connector.Connector{}
			for i, status := range tt.statuses {
				src := runtimeSource("fake-" + string(rune('a'+i)))
				sources = append(sources, src)
				fake := connector.NewFake(src)
				fake.Status = status
				conns[src.ID] = fake
			}
			runtime, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{
				Sources:  sources,
				Registry: registryOf(t, conns),
				Sink:     &connector.Recorder{},
			})
			if err != nil {
				t.Fatalf("NewRuntime() = %v, want no error", err)
			}
			defer func() { _ = runtime.Close(t.Context()) }()

			health := runtime.Health(t.Context())
			if health.Status != tt.want {
				t.Errorf("Health().Status = %q, want %q", health.Status, tt.want)
			}
			if len(health.Sources) != len(tt.statuses) {
				t.Fatalf("Health() reports %d sources, want %d", len(health.Sources), len(tt.statuses))
			}
			for i, got := range health.Sources {
				// Each source keeps its own status, unchanged: the aggregate is
				// the runtime's summary, not a rewrite of what a connector said.
				if got.Status != tt.statuses[i] {
					t.Errorf("source %s reports %q, want the connector's own %q", got.Source, got.Status, tt.statuses[i])
				}
				if got.Source != sources[i].ID || got.Type != sources[i].Type {
					t.Errorf("Health() entry %d is %s/%s, want %s/%s", i, got.Source, got.Type, sources[i].ID, sources[i].Type)
				}
			}
		})
	}
}

// Building the connectors is where a misconfigured source fails: a source that
// cannot start is a startup failure and not a health status
// (docs/connector-contract.md).
func TestNewRuntimeRefusesWhatCannotRun(t *testing.T) {
	src := runtimeSource("fake-eng")
	fakes := registryOf(t, map[string]connector.Connector{src.ID: connector.NewFake(src)})
	tests := []struct {
		name string
		opts connector.RuntimeOptions
		want string
	}{
		{
			name: "no sink",
			opts: connector.RuntimeOptions{Registry: connector.NewRegistry()},
			want: "needs a sink",
		},
		{
			name: "no registry",
			opts: connector.RuntimeOptions{Sink: &connector.Recorder{}},
			want: "needs a registry",
		},
		{
			name: "a type no factory claims",
			opts: connector.RuntimeOptions{
				Sources:  []connector.SourceConfig{{ID: "github", Type: "github", Containers: []string{"acme/api"}}},
				Registry: connector.NewRegistry(),
				Sink:     &connector.Recorder{},
			},
			want: "unknown connector type",
		},
		{
			name: "one source configured twice",
			opts: connector.RuntimeOptions{
				Sources:  []connector.SourceConfig{src, src},
				Registry: fakes,
				Sink:     &connector.Recorder{},
			},
			want: "configured twice",
		},
		{
			name: "a secret that is not in the environment",
			opts: connector.RuntimeOptions{
				Sources: []connector.SourceConfig{{
					ID: "fake-eng", Type: connector.FakeType, Containers: []string{"C123"},
					Secrets: map[string]string{"token": "HEARSAY_TEST_TOKEN_THAT_IS_NOT_SET"},
				}},
				Registry: fakes,
				Sink:     &connector.Recorder{},
				Lookup:   func(string) (string, bool) { return "", false },
			},
			want: "HEARSAY_TEST_TOKEN_THAT_IS_NOT_SET",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := connector.NewRuntime(t.Context(), tt.opts)
			if err == nil {
				_ = runtime.Close(t.Context())
				t.Fatalf("NewRuntime() = nil, want an error about %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("NewRuntime() = %v, want an error about %q", err, tt.want)
			}
		})
	}
}

// A connector already built when a later source fails is closed: nothing else
// holds it, so nothing else can.
func TestNewRuntimeClosesWhatItBuiltBeforeItFailed(t *testing.T) {
	first := runtimeSource("fake-one")
	built := connector.NewFake(first)
	_, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{
		Sources: []connector.SourceConfig{first, {ID: "github", Type: "github", Containers: []string{"acme/api"}}},
		Registry: registryOf(t, map[string]connector.Connector{
			first.ID: built,
		}),
		Sink: &connector.Recorder{},
	})
	if err == nil {
		t.Fatal("NewRuntime() = nil, want an error for the unknown type")
	}
	if err := built.Poll(t.Context(), &connector.Recorder{}); !errors.Is(err, connector.ErrClosed) {
		t.Errorf("the connector built before the failure is still open: Poll() = %v, want ErrClosed", err)
	}
}

// Configuration names an environment variable and the runtime supplies its
// value (docs/config.md).
func TestResolveSecrets(t *testing.T) {
	env := map[string]string{"HEARSAY_GITHUB_TOKEN": "t0ken", "HEARSAY_EMPTY": ""}
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
	tests := []struct {
		name    string
		secrets map[string]string
		want    map[string]string
		wantErr string
	}{
		{name: "no secrets at all", secrets: nil},
		{
			name:    "a variable that is set",
			secrets: map[string]string{"token": "HEARSAY_GITHUB_TOKEN"},
			want:    map[string]string{"token": "t0ken"},
		},
		{
			name:    "a variable that is not set",
			secrets: map[string]string{"token": "HEARSAY_MISSING"},
			wantErr: "token (HEARSAY_MISSING)",
		},
		{
			name:    "a variable that is set to nothing",
			secrets: map[string]string{"token": "HEARSAY_EMPTY"},
			wantErr: "token (HEARSAY_EMPTY)",
		},
		{
			name:    "every missing variable at once",
			secrets: map[string]string{"token": "HEARSAY_MISSING", "webhook_secret": "HEARSAY_ALSO_MISSING"},
			wantErr: "token (HEARSAY_MISSING) webhook_secret (HEARSAY_ALSO_MISSING)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := connector.SourceConfig{ID: "github", Type: "github", Secrets: tt.secrets}
			got, err := connector.ResolveSecrets(src, lookup)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveSecrets() = %v, want an error naming %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "t0ken") {
					t.Error("the error carries a secret's value")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSecrets() = %v, want no error", err)
			}
			for name, want := range tt.want {
				if got.Secrets[name] != want {
					t.Errorf("secret %q = %q, want %q", name, got.Secrets[name], want)
				}
			}
			if len(got.Secrets) != len(tt.want) {
				t.Errorf("ResolveSecrets() returned %d secrets, want %d", len(got.Secrets), len(tt.want))
			}
		})
	}
}

// The floor, the fallback and the cap are the runtime's own, applied on top of
// what a source asked for.
func TestCadence(t *testing.T) {
	cadence := connector.Cadence{MinRefresh: 30 * time.Second, Refresh: 5 * time.Minute, MaxBackoff: 15 * time.Minute}

	t.Run("interval", func(t *testing.T) {
		tests := []struct {
			name string
			src  time.Duration
			want time.Duration
		}{
			{name: "what the source asked for", src: 2 * time.Minute, want: 2 * time.Minute},
			{name: "under the floor", src: time.Second, want: 30 * time.Second},
			{name: "no refresh in config", src: 0, want: 5 * time.Minute},
			{name: "exactly the floor", src: 30 * time.Second, want: 30 * time.Second},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got := cadence.Interval(connector.SourceConfig{ID: "s", Refresh: tt.src})
				if got != tt.want {
					t.Errorf("Interval(%s) = %s, want %s", tt.src, got, tt.want)
				}
			})
		}
	})

	t.Run("a tick is spread over the interval after it is due", func(t *testing.T) {
		// Two sources, or two replicas, whose ticks start together stay in
		// lockstep forever without this.
		seen := map[time.Duration]bool{}
		for range 200 {
			seen[cadence.Backoff(time.Minute, 1)] = true
		}
		if len(seen) < 2 {
			t.Errorf("200 ticks of a one-minute interval all came out at %v, want them spread", slices.Collect(maps.Keys(seen)))
		}
	})

	t.Run("backoff", func(t *testing.T) {
		tests := []struct {
			name     string
			interval time.Duration
			fails    int
			want     time.Duration
		}{
			{name: "the first failure waits an interval", interval: time.Minute, fails: 1, want: time.Minute},
			{name: "the second doubles it", interval: time.Minute, fails: 2, want: 2 * time.Minute},
			{name: "the third doubles again", interval: time.Minute, fails: 3, want: 4 * time.Minute},
			{name: "and it is capped", interval: time.Minute, fails: 30, want: 15 * time.Minute},
			{name: "a source slower than the cap keeps its own cadence", interval: time.Hour, fails: 30, want: time.Hour},
			{name: "zero failures is the interval", interval: time.Minute, fails: 0, want: time.Minute},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// Jitter is added on top and never taken off: the interval is a
				// promise not to poll a source more often than that.
				for range 50 {
					got := cadence.Backoff(tt.interval, tt.fails)
					if got < tt.want || got > tt.want+tt.want/10 {
						t.Fatalf("Backoff(%s, %d) = %s, want between %s and %s", tt.interval, tt.fails, got, tt.want, tt.want+tt.want/10)
					}
				}
			})
		}
	})
}

// artifacts is what reached a sink, which is what a test compares.
func artifacts(rec *connector.Recorder) []string {
	out := []string{}
	for _, ev := range rec.Events() {
		out = append(out, ev.Payload.Artifact)
	}
	return out
}
