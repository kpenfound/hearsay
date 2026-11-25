package connector_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/poll"
)

type walker struct {
	*connector.Fake
	pages int

	mu         sync.Mutex
	calls      []string
	asked      []string
	private    map[string]bool
	publicErr  error
	sink       connector.Sink
	fail       map[string]error
	stuck      map[string]bool
	during     func()
	duringAt   connector.Cursor
	duringOnce sync.Once
}

func newWalker(src connector.SourceConfig, pages int) *walker {
	return &walker{Fake: connector.NewFake(src), pages: pages, private: map[string]bool{}, fail: map[string]error{}, stuck: map[string]bool{}}
}

func (w *walker) Handler(sink connector.Sink) http.Handler {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sink = sink
	return w.Fake.Handler(sink)
}

func (w *walker) Public(_ context.Context, container string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, container)
	if w.publicErr != nil {
		return false, w.publicErr
	}
	return !w.private[container], nil
}

func (w *walker) Resync(_ context.Context, _ connector.Sink, container string, from connector.Cursor) (connector.BackfillResult, error) {
	w.mu.Lock()
	w.calls = append(w.calls, container+"@"+string(from))
	during := w.during
	fail, stuck := w.fail[container], w.stuck[container]
	w.mu.Unlock()
	if fail != nil {
		return connector.BackfillResult{}, fail
	}
	if stuck {
		return connector.BackfillResult{Next: from}, nil
	}
	if during != nil && from == w.duringAt {
		w.duringOnce.Do(during)
	}
	n := 0
	if from != "" {
		n, _ = strconv.Atoi(string(from))
	}
	if n+1 >= w.pages {
		return connector.BackfillResult{Done: true, Events: 1}, nil
	}
	return connector.BackfillResult{Next: connector.Cursor(strconv.Itoa(n + 1)), Events: 1}, nil
}

func (w *walker) setPublicErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.publicErr = err
}

func (w *walker) callsSoFar() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

func (w *walker) askedSoFar() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.asked)
}

func (w *walker) requester(t *testing.T) connector.ResyncRequester {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	req, ok := w.sink.(connector.ResyncRequester)
	if !ok {
		t.Fatalf("the sink the runtime handed the connector is %T, which records no re-sync", w.sink)
	}
	return req
}

type exposure []connector.Exposure

func (e exposure) Exposed(context.Context, string) ([]connector.Exposure, error) { return e, nil }

type idleSignal chan struct{}

func (s idleSignal) Exposed(context.Context, string) ([]connector.Exposure, error) {
	close(s)
	return nil, nil
}

func settled(store *connector.MemoryResyncs, source, container string) func() bool {
	return func() bool {
		r := store.Get(source, container)
		return r.Generation > 0 && !r.Owed
	}
}

func TestAnOwedResyncResumesFromItsStoredCursor(t *testing.T) {
	src := runtimeSource("resume")
	w := newWalker(src, 4)
	resyncs := connector.NewMemoryResyncs()
	resyncs.Set(src.ID, connector.Resync{Container: "C123", Owed: true, Cursor: "2", Generation: 1})
	_, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs,
	})
	poll.Until(t, "the re-sync to finish", settled(resyncs, src.ID, "C123"))
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got, want := w.callsSoFar(), []string{"C123@2", "C123@3"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if rec := resyncs.Get(src.ID, "C123"); rec.ResyncedAt.IsZero() || rec.Cursor != "" {
		t.Errorf("record after the re-sync = %+v, want a finish time and no cursor", rec)
	}
}

func TestARequestedResyncIsRecordedAndRun(t *testing.T) {
	src := runtimeSource("request")
	w := newWalker(src, 2)
	resyncs := connector.NewMemoryResyncs()
	idle := make(idleSignal)
	_, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs, Exposure: idle,
	})
	select {
	case <-idle:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the runtime to finish its startup check")
	}
	if err := w.requester(t).RequestResync(t.Context(), "C123"); err != nil {
		t.Fatalf("RequestResync(C123) = %v", err)
	}
	poll.Until(t, "the re-sync to finish", settled(resyncs, src.ID, "C123"))
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got, want := w.callsSoFar(), []string{"C123@", "C123@1"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestARequestedResyncOfAContainerConfigDoesNotAllowOwesNothing(t *testing.T) {
	src := runtimeSource("request")
	w := newWalker(src, 2)
	resyncs := connector.NewMemoryResyncs()
	idle := make(idleSignal)
	_, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs, Exposure: idle,
	})
	select {
	case <-idle:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the runtime to finish its startup check")
	}
	if err := w.requester(t).RequestResync(t.Context(), "elsewhere"); err != nil {
		t.Errorf("RequestResync(elsewhere) = %v, want nil", err)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if rec := resyncs.Get(src.ID, "elsewhere"); rec.Generation != 0 {
		t.Errorf("a container config does not allow was recorded: %+v", rec)
	}
	if got := w.callsSoFar(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

func TestARequestedResyncWithNoStoreIsRefused(t *testing.T) {
	src := runtimeSource("request")
	w := newWalker(src, 2)
	_, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{},
	})
	defer func() { _ = stop() }()
	if err := w.requester(t).RequestResync(t.Context(), "C123"); !errors.Is(err, connector.ErrNoResyncStore) {
		t.Errorf("RequestResync with no store = %v, want ErrNoResyncStore", err)
	}
}

func TestARequestDuringAWalkStartsItOver(t *testing.T) {
	src := runtimeSource("again")
	w := newWalker(src, 3)
	resyncs := connector.NewMemoryResyncs()
	w.duringAt = "1"
	w.during = func() {
		if err := resyncs.Owe(context.Background(), src.ID, "C123"); err != nil {
			t.Errorf("Owe = %v", err)
		}
	}
	resyncs.Set(src.ID, connector.Resync{Container: "C123", Owed: true, Generation: 1})
	_, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs,
	})
	poll.Until(t, "the re-sync to finish", settled(resyncs, src.ID, "C123"))
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got, want := w.callsSoFar(), []string{"C123@", "C123@1", "C123@", "C123@1", "C123@2"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestAnOwedResyncOfAContainerConfigDroppedIsNotWalked(t *testing.T) {
	src := runtimeSource("dropped")
	w := newWalker(src, 1)
	resyncs := connector.NewMemoryResyncs()
	resyncs.Set(src.ID, connector.Resync{Container: "B000", Owed: true, Generation: 1})
	resyncs.Set(src.ID, connector.Resync{Container: "C123", Owed: true, Generation: 1})
	_, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs,
	})
	poll.Until(t, "the re-sync of C123", settled(resyncs, src.ID, "C123"))
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got, want := w.callsSoFar(), []string{"C123@"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestTheStartupCheckOwesAResyncForAContainerNoLongerPublic(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		container string
		private   bool
		resynced  time.Time
		wantAsked bool
		wantOwed  bool
	}{
		{name: "private at the source", container: "C123", private: true, wantAsked: true, wantOwed: true},
		{name: "still public at the source", container: "C123", wantAsked: true},
		{name: "not allowed by config", container: "B000", private: true},
		{name: "re-synced after its last public artifact", container: "C123", private: true, resynced: now.Add(time.Minute)},
		{name: "public again after its last re-sync", container: "C123", private: true, resynced: now.Add(-time.Minute), wantAsked: true, wantOwed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := runtimeSource("check")
			src.Containers = []string{"C123", "Z999"}
			w := newWalker(src, 1)
			w.private[tt.container] = tt.private
			w.private["Z999"] = true
			resyncs := connector.NewMemoryResyncs()
			if !tt.resynced.IsZero() {
				resyncs.Set(src.ID, connector.Resync{Container: tt.container, Generation: 1, ResyncedAt: tt.resynced})
			}
			_, stop := start(t, connector.RuntimeOptions{
				Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
				Sink: &connector.Recorder{}, Resyncs: resyncs,
				Exposure: exposure{{Container: tt.container, LastPublic: now}, {Container: "Z999", LastPublic: now}},
			})
			poll.Until(t, "the sentinel's re-sync", settled(resyncs, src.ID, "Z999"))
			if tt.wantOwed {
				poll.Until(t, "the re-sync of "+tt.container, func() bool {
					r := resyncs.Get(src.ID, tt.container)
					return r.Generation > 0 && !r.Owed && r.ResyncedAt.After(now)
				})
			}
			if err := stop(); err != nil {
				t.Fatalf("Run() = %v", err)
			}
			if asked := slices.Contains(w.askedSoFar(), tt.container); asked != tt.wantAsked {
				t.Errorf("asked about %s = %v, want %v (asked %v)", tt.container, asked, tt.wantAsked, w.askedSoFar())
			}
			if walked := slices.Contains(w.callsSoFar(), tt.container+"@"); walked != tt.wantOwed {
				t.Errorf("walked %s = %v, want %v (calls %v)", tt.container, walked, tt.wantOwed, w.callsSoFar())
			}
		})
	}
}

func TestAFailingResyncIsCountedAndRetried(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		fail func(*walker, *connector.MemoryResyncs, error)
	}{
		{"the store", func(_ *walker, s *connector.MemoryResyncs, err error) { s.SetErr(err) }},
		{"the connector's answer", func(w *walker, _ *connector.MemoryResyncs, err error) { w.setPublicErr(err) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := runtimeSource("failing")
			w := newWalker(src, 1)
			w.private["C123"] = true
			resyncs := connector.NewMemoryResyncs()
			tt.fail(w, resyncs, boom)
			rt, stop := start(t, connector.RuntimeOptions{
				Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
				Sink: &connector.Recorder{}, Resyncs: resyncs, Exposure: exposure{{Container: "C123", LastPublic: time.Now()}},
			})
			poll.Until(t, "failures in health", func() bool { return rt.Health(t.Context()).Sources[0].ResyncFailures > 1 })
			tt.fail(w, resyncs, nil)
			poll.Until(t, "the re-sync", settled(resyncs, src.ID, "C123"))
			poll.Until(t, "the count to clear", func() bool { return rt.Health(t.Context()).Sources[0].ResyncFailures == 0 })
			if err := stop(); err != nil {
				t.Fatalf("Run() = %v", err)
			}
		})
	}
}

func (w *walker) callsTo(container string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.calls {
		if len(c) > len(container) && c[:len(container)+1] == container+"@" {
			n++
		}
	}
	return n
}

func TestAResyncThatMakesNoProgressIsNotProgress(t *testing.T) {
	src := runtimeSource("stuck")
	w := newWalker(src, 4)
	w.stuck["C123"] = true
	resyncs := connector.NewMemoryResyncs()
	resyncs.Set(src.ID, connector.Resync{Container: "C123", Owed: true, Cursor: "2", Generation: 1})
	rt, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs,
	})
	poll.Until(t, "the re-sync to be tried again", func() bool { return w.callsTo("C123") >= 2 })
	poll.Until(t, "the failure in health", func() bool { return rt.Health(t.Context()).Sources[0].ResyncFailures >= 1 })
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if rec := resyncs.Get(src.ID, "C123"); !rec.Owed || rec.Cursor != "2" || !rec.ResyncedAt.IsZero() {
		t.Errorf("record after a stuck re-sync = %+v, want still owed at cursor 2", rec)
	}
}

func TestAFailingResyncHoldsUpNothingElse(t *testing.T) {
	src := runtimeSource("blocked")
	src.Containers = []string{"A000", "C123", "D456"}
	w := newWalker(src, 3)
	w.fail["A000"] = errors.New("repository not found")
	w.private["D456"] = true
	resyncs := connector.NewMemoryResyncs()
	resyncs.Set(src.ID, connector.Resync{Container: "A000", Owed: true, Generation: 1})
	resyncs.Set(src.ID, connector.Resync{Container: "C123", Owed: true, Generation: 1})
	rt, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs, Exposure: exposure{{Container: "D456", LastPublic: time.Now()}},
	})
	poll.Until(t, "the re-sync of C123", settled(resyncs, src.ID, "C123"))
	poll.Until(t, "the re-sync the check found", settled(resyncs, src.ID, "D456"))
	poll.Until(t, "A000's failures in health", func() bool { return rt.Health(t.Context()).Sources[0].ResyncFailures >= 1 })
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if rec := resyncs.Get(src.ID, "A000"); !rec.Owed {
		t.Errorf("A000 = %+v, want still owed", rec)
	}
}

func TestARequestDuringABackoffIsNotKeptWaiting(t *testing.T) {
	src := runtimeSource("backoff")
	src.Containers = []string{"A000", "C123", "D456"}
	src.Refresh = time.Hour
	w := newWalker(src, 2)
	w.fail["A000"] = errors.New("repository not found")
	w.private["D456"] = true
	resyncs := connector.NewMemoryResyncs()
	resyncs.Set(src.ID, connector.Resync{Container: "A000", Owed: true, Generation: 1})
	rt, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs, Exposure: exposure{{Container: "D456", LastPublic: time.Now()}},
		Cadence: connector.Cadence{MinRefresh: time.Millisecond, MaxBackoff: time.Hour, Shutdown: time.Second},
	})
	poll.Until(t, "the re-sync the check found", settled(resyncs, src.ID, "D456"))
	poll.Until(t, "the runtime to back off", func() bool { return rt.Health(t.Context()).Sources[0].ResyncFailures >= 1 })
	if err := w.requester(t).RequestResync(t.Context(), "C123"); err != nil {
		t.Fatalf("RequestResync(C123) = %v", err)
	}
	poll.Until(t, "the requested re-sync", settled(resyncs, src.ID, "C123"))
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
}
