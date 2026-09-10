package connectors_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/service/connectors"
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

// Writing L0 is what a connector does, so a process with no database is one
// that cannot do its job.
func TestTheServiceNeedsSomewhereToWrite(t *testing.T) {
	err := connectors.Run(t.Context(), &config.Config{}, connectors.Deps{})
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("Run() with no database and no sink = %v, want an error about the database", err)
	}
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

// The `source` log field says which connectors a process is hosting.
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
