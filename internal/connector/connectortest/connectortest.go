// Package connectortest holds the test support the connector packages share:
// hosting a push connector, running a runtime, and a sink that drops
// redeliveries.
package connectortest

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/service/connectors"
)

// Serve hosts c behind a gate that records into the returned Recorder, on a test
// server closed when the test ends.
func Serve(t testing.TB, src connector.SourceConfig, c connector.Pusher) (*httptest.Server, *connector.Recorder) {
	t.Helper()
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	srv := httptest.NewServer(c.Handler(gate))
	t.Cleanup(srv.Close)
	return srv, rec
}

// Post sends body to url with header and returns the status and the answer.
func Post(t testing.TB, url string, header http.Header, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(out)
}

// Bearer is the header of a request authenticated by token. An empty token sends
// no credential.
func Bearer(token string) http.Header {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

// PostWithToken is [Post] with a bearer token and no other header.
func PostWithToken(t testing.TB, url, token, body string) (int, string) {
	t.Helper()
	return Post(t, url, Bearer(token), body)
}

// Lookup is an environment lookup over env.
func Lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// HostedSource runs src through the connectors service with factory registered
// for typ, posts body to its hook with token, and checks that the service
// reports the source ready with an event taken.
func HostedSource(t testing.TB, src connector.SourceConfig, typ string, factory connector.Factory, token, body string) {
	t.Helper()
	registry := connector.NewRegistry()
	if err := registry.Register(typ, factory); err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + listener.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- connectors.Run(ctx, &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}, connectors.Deps{
			Registry: registry,
			Sink:     &connector.Recorder{},
			Cursors:  connector.NewMemoryCursors(),
			Listener: listener,
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v", err)
		}
	}()

	if code, out := Post(t, addr+connector.HookPath(src.ID), Bearer(token), body); code != http.StatusAccepted {
		t.Fatalf("POST %s = %d %s, want 202", connector.HookPath(src.ID), code, out)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, addr+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var ready connectors.Readiness
	if err := json.NewDecoder(resp.Body).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || ready.Status != connector.HealthOK {
		t.Errorf("GET /readyz = %d %+v, want 200 and ok", resp.StatusCode, ready)
	}
	if len(ready.Sources) != 1 || ready.Sources[0].Source != src.ID || ready.Sources[0].Type != typ {
		t.Fatalf("readiness lists %+v, want the %s source", ready.Sources, typ)
	}
	if ready.Sources[0].LastEventAt.IsZero() {
		t.Error("readiness has no last event time after an event was taken")
	}
}

// Run builds a runtime for src with factory registered for typ, a fast cadence
// and a Recorder sink, lets configure change its options, and runs it until the
// test ends. stop ends it sooner and waits for Run to return.
func Run(t testing.TB, typ string, factory connector.Factory, src connector.SourceConfig, lookup func(string) (string, bool), configure func(*connector.RuntimeOptions)) (rt *connector.Runtime, stop func()) {
	t.Helper()
	registry := connector.NewRegistry()
	if err := registry.Register(typ, factory); err != nil {
		t.Fatal(err)
	}
	opts := connector.RuntimeOptions{
		Sources:  []connector.SourceConfig{src},
		Registry: registry,
		Sink:     &connector.Recorder{},
		Lookup:   lookup,
		Cadence:  connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second},
	}
	configure(&opts)
	rt, err := connector.NewRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the runtime did not stop within 10s")
		}
	})
	t.Cleanup(stop)
	return rt, stop
}

// Deduplicating is a sink that keeps the first delivery of each event id and
// counts the rest, as L0 does.
type Deduplicating struct {
	mu         sync.Mutex
	recorder   connector.Recorder
	seen       map[string]bool
	deliveries []connector.Event
	changed    chan struct{}
}

// NewDeduplicating returns an empty Deduplicating sink.
func NewDeduplicating() *Deduplicating {
	return &Deduplicating{seen: map[string]bool{}, changed: make(chan struct{})}
}

// Emit implements [connector.Sink].
func (s *Deduplicating) Emit(ctx context.Context, ev connector.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		close(s.changed)
		s.changed = make(chan struct{})
	}()
	s.deliveries = append(s.deliveries, ev)
	if s.seen[ev.ID] {
		return nil
	}
	if err := s.recorder.Emit(ctx, ev); err != nil {
		return err
	}
	s.seen[ev.ID] = true
	return nil
}

// Events are the events kept, in the order they first arrived.
func (s *Deduplicating) Events() []connector.Event { return s.recorder.Events() }

// Deliveries are every event delivered, repeats included.
func (s *Deduplicating) Deliveries() []connector.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deliveries)
}

// DeliveredIDs are the ids of Deliveries.
func (s *Deduplicating) DeliveredIDs() []string {
	var ids []string
	for _, ev := range s.Deliveries() {
		ids = append(ids, ev.ID)
	}
	return ids
}

// Await returns once cond holds, rechecking after every delivery, and fails the
// test if it does not within five seconds.
func (s *Deduplicating) Await(t testing.TB, what string, cond func() bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		s.mu.Lock()
		changed := s.changed
		s.mu.Unlock()
		if cond() {
			return
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Finish carries on a backfill from the position stored for source: it calls
// backfill with the stored cursor, stores the position each call returns, and
// stops when a call reports it done.
func Finish(t testing.TB, cursors connector.CursorStore, source string, backfill func(cursor connector.Cursor) (connector.BackfillResult, error)) {
	t.Helper()
	state, err := cursors.Load(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	for !state.Done {
		result, err := backfill(state.Cursor)
		if err != nil {
			t.Fatal(err)
		}
		state.Cursor = result.Next
		state.Done = result.Done
		state.Events += int64(result.Events)
		if err := cursors.Save(t.Context(), source, state); err != nil {
			t.Fatal(err)
		}
	}
}
