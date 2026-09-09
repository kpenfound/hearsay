package connector_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
)

// The fake is the connector the rest of the repository's tests drive, so it has
// to implement every part of the contract a real one does.
var (
	_ connector.Connector  = (*connector.Fake)(nil)
	_ connector.Poller     = (*connector.Fake)(nil)
	_ connector.Pusher     = (*connector.Fake)(nil)
	_ connector.Backfiller = (*connector.Fake)(nil)
	_ connector.Sink       = (*connector.Recorder)(nil)
)

func fakeSource() connector.SourceConfig {
	return connector.SourceConfig{ID: "fake-eng", Type: connector.FakeType, Containers: []string{"C123"}}
}

// A fake wired to the allowlist built from the same source config produces
// events that pass the gate: a test that wants a working ingest path gets one
// without hand-building an event.
func TestFakePollEmitsThroughTheGate(t *testing.T) {
	src := fakeSource()
	fake := connector.NewFake(src)
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, fake.Describe(), connector.NewAllowlist(src))

	fake.Queue = []connector.Event{
		fake.NewEvent(connector.KindMessage, "m1", "we should hand-run the migration"),
		fake.NewEvent(connector.KindMessage, "m2", "agreed"),
	}
	if err := fake.Poll(t.Context(), gate); err != nil {
		t.Fatalf("Poll() = %v, want no error", err)
	}

	want := []string{connector.EventID(src.ID, "m1"), connector.EventID(src.ID, "m2")}
	if got := rec.IDs(); !slices.Equal(got, want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	// Poll drains: the second pass re-emits nothing.
	if err := fake.Poll(t.Context(), gate); err != nil {
		t.Fatalf("second Poll() = %v, want no error", err)
	}
	if got := len(rec.Events()); got != 2 {
		t.Errorf("recorded %d events after a second poll, want 2", got)
	}
	if h := fake.Health(t.Context()); h.Status != connector.HealthOK || h.LastEventAt.IsZero() {
		t.Errorf("Health() = %+v, want ok with a last event time", h)
	}
}

func TestFakePollReturnsItsError(t *testing.T) {
	fake := connector.NewFake(fakeSource())
	wantErr := errors.New("rate limited")
	fake.PollErr = wantErr
	fake.Queue = []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "hello")}

	rec := &connector.Recorder{}
	if err := fake.Poll(t.Context(), rec); !errors.Is(err, wantErr) {
		t.Fatalf("Poll() = %v, want %v", err, wantErr)
	}
	if got := len(rec.Events()); got != 0 {
		t.Errorf("recorded %d events, want none", got)
	}
}

func TestFakeBackfillWalksPagesWithACursor(t *testing.T) {
	fake := connector.NewFake(fakeSource())
	fake.Pages = [][]connector.Event{
		{fake.NewEvent(connector.KindMessage, "m1", "one")},
		{fake.NewEvent(connector.KindMessage, "m2", "two"), fake.NewEvent(connector.KindMessage, "m3", "three")},
	}
	rec := &connector.Recorder{}

	var cursor connector.Cursor
	for pass := range 2 {
		res, err := fake.Backfill(t.Context(), rec, cursor)
		if err != nil {
			t.Fatalf("Backfill(pass %d) = %v, want no error", pass, err)
		}
		if res.Events != len(fake.Pages[pass]) {
			t.Errorf("Backfill(pass %d).Events = %d, want %d", pass, res.Events, len(fake.Pages[pass]))
		}
		if wantDone := pass == 1; res.Done != wantDone {
			t.Errorf("Backfill(pass %d).Done = %v, want %v", pass, res.Done, wantDone)
		}
		cursor = res.Next
	}
	if got, want := len(rec.Events()), 3; got != want {
		t.Fatalf("recorded %d events, want %d", got, want)
	}

	// A backfill that has already finished emits nothing more, which is what
	// lets the runtime keep a cursor and stop.
	res, err := fake.Backfill(t.Context(), rec, cursor)
	if err != nil {
		t.Fatalf("Backfill() past the end = %v, want no error", err)
	}
	if !res.Done || len(rec.Events()) != 3 {
		t.Errorf("Backfill() past the end = %+v with %d events recorded, want done and 3", res, len(rec.Events()))
	}
}

func TestFakeHandlerEmitsWhatIsPostedToIt(t *testing.T) {
	src := fakeSource()
	fake := connector.NewFake(src)
	rec := &connector.Recorder{}
	handler := fake.Handler(connector.NewGate(rec, src.ID, fake.Describe(), connector.NewAllowlist(src)))

	// Written out rather than marshalled from an Event: what a push connector
	// receives is JSON from somewhere else, and this pins the field names a
	// connector in another language has to produce.
	body := `{"source":"fake-eng","native_id":"m1","kind":"message","time":"2026-09-09T12:00:00Z",` +
		`"payload":{"artifact":"m1","container":{"kind":"channel","native_id":"C123"},"text":"one",` +
		`"author":{"source":"fake-eng","kind":"user","native_id":"u1"}},"acl":[{"kind":"public"}]}`

	tests := []struct {
		name       string
		method     string
		body       string
		wantStatus int
		wantEvents int
	}{
		{name: "one event", method: http.MethodPost, body: body, wantStatus: http.StatusAccepted, wantEvents: 1},
		{name: "an array of events", method: http.MethodPost, body: "[" + body + "]", wantStatus: http.StatusAccepted, wantEvents: 1},
		{name: "not json", method: http.MethodPost, body: "nope", wantStatus: http.StatusBadRequest},
		{name: "the wrong method", method: http.MethodGet, body: "", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(rec.Events())
			req := httptest.NewRequest(tt.method, "/ingest/fake-eng", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (%s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if got := len(rec.Events()) - before; got != tt.wantEvents {
				t.Errorf("emitted %d events, want %d", got, tt.wantEvents)
			}
		})
	}
}

func TestFakeRefusesToIngestAfterClose(t *testing.T) {
	fake := connector.NewFake(fakeSource())
	fake.Queue = []connector.Event{fake.NewEvent(connector.KindMessage, "m1", "one")}
	fake.Pages = [][]connector.Event{{fake.NewEvent(connector.KindMessage, "m2", "two")}}
	if err := fake.Close(t.Context()); err != nil {
		t.Fatalf("Close() = %v, want no error", err)
	}

	rec := &connector.Recorder{}
	if err := fake.Poll(t.Context(), rec); !errors.Is(err, connector.ErrClosed) {
		t.Errorf("Poll() after Close = %v, want ErrClosed", err)
	}
	if _, err := fake.Backfill(t.Context(), rec, ""); !errors.Is(err, connector.ErrClosed) {
		t.Errorf("Backfill() after Close = %v, want ErrClosed", err)
	}
	if got := len(rec.Events()); got != 0 {
		t.Errorf("recorded %d events after Close, want none", got)
	}
}

func TestFakeNewEventIsValid(t *testing.T) {
	fake := connector.NewFake(fakeSource())
	ev := fake.NewEvent(connector.KindMessage, "m1", "hello")
	if err := ev.Validate(); err != nil {
		t.Fatalf("NewEvent(...).Validate() = %v, want no error", err)
	}
	if ev.Payload.Container.NativeID != "C123" {
		t.Errorf("container = %q, want the source's first container", ev.Payload.Container.NativeID)
	}
}

// A source whose only container is the wildcard has no container id to put on
// an event, so the fake falls back to the source itself.
func TestFakeNewEventUnderTheWildcard(t *testing.T) {
	src := connector.SourceConfig{ID: "agent-sessions", Type: connector.FakeType, Containers: []string{connector.AllowAll}}
	fake := connector.NewFake(src)
	ev := fake.NewEvent(connector.KindAgentTurn, "s1:turn:1", "ran the tests")

	if err := ev.Validate(); err != nil {
		t.Fatalf("NewEvent(...).Validate() = %v, want no error", err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, fake.Describe(), connector.NewAllowlist(src))
	if err := gate.Emit(t.Context(), ev); err != nil {
		t.Fatalf("Emit() = %v, want no error", err)
	}
	if got := len(rec.Events()); got != 1 {
		t.Errorf("recorded %d events, want 1", got)
	}
}

func TestRegistry(t *testing.T) {
	t.Run("builds the connector a source asks for", func(t *testing.T) {
		reg := connector.NewRegistry()
		if err := reg.Register(connector.FakeType, connector.FakeFactory); err != nil {
			t.Fatalf("Register() = %v, want no error", err)
		}
		c, err := reg.New(t.Context(), fakeSource())
		if err != nil {
			t.Fatalf("New() = %v, want no error", err)
		}
		if got := c.Describe().Type; got != connector.FakeType {
			t.Errorf("Describe().Type = %q, want %q", got, connector.FakeType)
		}
	})

	t.Run("refuses a second factory for a type", func(t *testing.T) {
		reg := connector.NewRegistry()
		if err := reg.Register(connector.FakeType, connector.FakeFactory); err != nil {
			t.Fatalf("Register() = %v, want no error", err)
		}
		if err := reg.Register(connector.FakeType, connector.FakeFactory); !errors.Is(err, connector.ErrDuplicateType) {
			t.Errorf("Register() twice = %v, want ErrDuplicateType", err)
		}
	})

	t.Run("refuses an unregistered type", func(t *testing.T) {
		reg := connector.NewRegistry()
		src := fakeSource()
		src.Type = "slack"
		if _, err := reg.New(t.Context(), src); !errors.Is(err, connector.ErrUnknownType) {
			t.Errorf("New() = %v, want ErrUnknownType", err)
		}
	})

	t.Run("refuses a connector that cannot ingest", func(t *testing.T) {
		reg := connector.NewRegistry()
		if err := reg.Register("inert", func(context.Context, connector.SourceConfig) (connector.Connector, error) {
			return inert{}, nil
		}); err != nil {
			t.Fatalf("Register() = %v, want no error", err)
		}
		src := fakeSource()
		src.Type = "inert"
		if _, err := reg.New(t.Context(), src); !errors.Is(err, connector.ErrNoIngestMode) {
			t.Errorf("New() = %v, want ErrNoIngestMode", err)
		}
	})

	t.Run("refuses a connector that describes itself as another type", func(t *testing.T) {
		reg := connector.NewRegistry()
		if err := reg.Register("mislabelled", func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
			f := connector.NewFake(src)
			f.Type = connector.FakeType
			return f, nil
		}); err != nil {
			t.Fatalf("Register() = %v, want no error", err)
		}
		src := fakeSource()
		src.Type = "mislabelled"
		if _, err := reg.New(t.Context(), src); err == nil {
			t.Error("New() = nil error, want an error about the descriptor")
		}
	})

	t.Run("refuses a source id that cannot appear in an event", func(t *testing.T) {
		reg := connector.NewRegistry()
		if err := reg.Register(connector.FakeType, connector.FakeFactory); err != nil {
			t.Fatalf("Register() = %v, want no error", err)
		}
		src := fakeSource()
		src.ID = "Fake Eng"
		if _, err := reg.New(t.Context(), src); err == nil {
			t.Error("New() = nil error, want an error about the source id")
		}
	})
}

func TestSourceConfigDecodeSettings(t *testing.T) {
	type settings struct {
		Org string `json:"org"`
	}
	tests := []struct {
		name     string
		settings string
		want     string
		wantErr  bool
	}{
		{name: "decodes", settings: `{"org":"acme"}`, want: "acme"},
		{name: "empty settings are not an error", settings: ""},
		{name: "an unknown field is a typo, not something to ignore", settings: `{"orgg":"acme"}`, wantErr: true},
		{name: "malformed json", settings: `{`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := connector.SourceConfig{ID: "github-acme", Settings: []byte(tt.settings)}
			var got settings
			err := src.DecodeSettings(&got)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DecodeSettings(%q) = nil error, want error", tt.settings)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeSettings(%q) = %v, want no error", tt.settings, err)
			}
			if got.Org != tt.want {
				t.Errorf("DecodeSettings(%q) decoded org %q, want %q", tt.settings, got.Org, tt.want)
			}
		})
	}
}

// inert implements Connector and neither ingest mode.
type inert struct{}

func (inert) Describe() connector.Descriptor { return connector.Descriptor{Type: "inert"} }
func (inert) Health(context.Context) connector.Health {
	return connector.Health{Status: connector.HealthOK}
}
func (inert) Close(context.Context) error { return nil }
