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
)

var (
	_ connector.ResyncStore     = (*connector.MemoryResyncs)(nil)
	_ connector.ExposureReader  = (*connector.Recorder)(nil)
	_ connector.ResyncRequester = (*connector.Gate)(nil)
)

// walker is a [connector.Resyncer] whose re-sync of a container is a walk of
// pages calls, the cursor being the number of the next one. It records every
// call, and captures the sink its handler was given, which is how a test asks
// for a re-sync the way a push connector does.
type walker struct {
	*connector.Fake
	pages int

	mu        sync.Mutex
	calls     []string
	asked     []string
	private   map[string]bool
	publicErr error
	sink      connector.Sink
	// during, if set, runs inside the call for this cursor, once.
	during     func()
	duringAt   connector.Cursor
	duringOnce sync.Once
}

func newWalker(src connector.SourceConfig, pages int) *walker {
	return &walker{Fake: connector.NewFake(src), pages: pages, private: map[string]bool{}}
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
	w.mu.Unlock()
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

// exposure is an [connector.ExposureReader] with a fixed answer.
type exposure []connector.Exposure

func (e exposure) Exposed(context.Context, string) ([]connector.Exposure, error) { return e, nil }

func settled(store *connector.MemoryResyncs, source, container string) func() bool {
	return func() bool {
		r := store.Get(source, container)
		return r.Generation > 0 && !r.Owed
	}
}

// A re-sync the store says is owed is resumed from its stored cursor, not
// started again, and finishing it settles the debt.
func TestAnOwedResyncResumesFromItsStoredCursor(t *testing.T) {
	src := runtimeSource("resume")
	w := newWalker(src, 4)
	resyncs := connector.NewMemoryResyncs()
	resyncs.Set(src.ID, connector.Resync{Container: "C123", Owed: true, Cursor: "2", Generation: 1})
	_, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs,
	})
	waitFor(t, "the re-sync to finish", settled(resyncs, src.ID, "C123"))
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

// A push connector asks for a re-sync through the sink it was handed, which
// records it before returning and wakes the runtime to walk it. A container
// config does not allow owes nothing, and without a store nothing is recorded
// and the connector is told.
func TestARequestedResyncIsRecordedAndRun(t *testing.T) {
	src := runtimeSource("request")
	w := newWalker(src, 2)
	resyncs := connector.NewMemoryResyncs()
	_, stop := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: w}),
		Sink: &connector.Recorder{}, Resyncs: resyncs,
	})
	req := w.requester(t)
	if err := req.RequestResync(t.Context(), "elsewhere"); err != nil {
		t.Errorf("RequestResync(a container config does not allow) = %v, want nil", err)
	}
	if err := req.RequestResync(t.Context(), "C123"); err != nil {
		t.Fatalf("RequestResync(C123) = %v", err)
	}
	waitFor(t, "the re-sync to finish", settled(resyncs, src.ID, "C123"))
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got, want := w.callsSoFar(), []string{"C123@", "C123@1"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if rec := resyncs.Get(src.ID, "elsewhere"); rec.Generation != 0 {
		t.Errorf("a container config does not allow was recorded: %+v", rec)
	}

	bare := newWalker(src, 2)
	_, stopBare := start(t, connector.RuntimeOptions{
		Sources: []connector.SourceConfig{src}, Registry: registryOf(t, map[string]connector.Connector{src.ID: bare}),
		Sink: &connector.Recorder{},
	})
	defer func() { _ = stopBare() }()
	if err := bare.requester(t).RequestResync(t.Context(), "C123"); !errors.Is(err, connector.ErrNoResyncStore) {
		t.Errorf("RequestResync with no store = %v, want ErrNoResyncStore", err)
	}
}

// A request that arrives while its container's walk is running starts the walk
// over, rather than being settled by a walk that began before it: part of that
// walk may have read the container before it changed. The page the old walk
// was reading does not move the new walk's cursor.
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
	waitFor(t, "the re-sync to finish", settled(resyncs, src.ID, "C123"))
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got, want := w.callsSoFar(), []string{"C123@", "C123@1", "C123@", "C123@1", "C123@2"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// An owed re-sync of a container config no longer allows is not walked, and
// does not stand in the way of one that is.
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
	waitFor(t, "the re-sync of C123", settled(resyncs, src.ID, "C123"))
	if err := stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got, want := w.callsSoFar(), []string{"C123@"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// At startup the runtime asks the connector about every container L0 serves as
// public, unless config does not allow it or it has been re-synced since the
// last public artifact arrived, and owes a re-sync for each the source says is
// private. The sentinel container, last in the answer and always private, is
// how the test knows the check has decided about the one before it.
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
			waitFor(t, "the sentinel's re-sync", settled(resyncs, src.ID, "Z999"))
			if tt.wantOwed {
				waitFor(t, "the re-sync of "+tt.container, func() bool {
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

// A re-sync that fails — the store, or the connector's answer — is counted in
// health and retried, and the count clears once it works.
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
			waitFor(t, "failures in health", func() bool { return rt.Health(t.Context()).Sources[0].ResyncFailures > 1 })
			tt.fail(w, resyncs, nil)
			waitFor(t, "the re-sync", settled(resyncs, src.ID, "C123"))
			waitFor(t, "the count to clear", func() bool { return rt.Health(t.Context()).Sources[0].ResyncFailures == 0 })
			if err := stop(); err != nil {
				t.Fatalf("Run() = %v", err)
			}
		})
	}
}

// The recorder answers which containers are exposed the way L0 does: by each
// artifact's current revision, past retracted artifacts and tombstones, for
// one source.
func TestRecorderExposed(t *testing.T) {
	src := runtimeSource("exposed")
	fake := connector.NewFake(src)
	private := func(ev connector.Event) connector.Event {
		ev.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: src.ID, NativeID: "C123"}}
		ev.NativeID += "@perm:private"
		return ev
	}
	in := func(ev connector.Event, container string) connector.Event {
		ev.Payload.Container.NativeID = container
		return ev
	}
	tombstone := fake.NewEvent(connector.KindTombstone, "gone:tombstone", "")
	tombstone.Payload.Target = "gone"
	other := connector.NewFake(runtimeSource("other"))

	tests := []struct {
		name   string
		events []connector.Event
		want   []string
	}{
		{"a public artifact", []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "x")}, []string{"C123"}},
		{"re-emitted private", []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "x"), private(fake.NewEvent(connector.KindMessage, "m1", "x"))}, nil},
		{"private, then public again", []connector.Event{private(fake.NewEvent(connector.KindMessage, "m1", "x")), fake.NewEvent(connector.KindMessage, "m1", "x")}, []string{"C123"}},
		{"retracted", []connector.Event{fake.NewEvent(connector.KindMessage, "gone", "x"), tombstone}, nil},
		{"another source's", []connector.Event{other.NewEvent(connector.KindMessage, "m1", "x")}, nil},
		{"two containers", []connector.Event{in(fake.NewEvent(connector.KindMessage, "m2", "x"), "D456"), fake.NewEvent(connector.KindMessage, "m1", "x")}, []string{"C123", "D456"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &connector.Recorder{}
			for _, ev := range tt.events {
				if err := rec.Emit(t.Context(), ev); err != nil {
					t.Fatal(err)
				}
			}
			exposed, err := rec.Exposed(t.Context(), src.ID)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range exposed {
				got = append(got, e.Container)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Exposed = %v, want %v", got, tt.want)
			}
		})
	}
}
