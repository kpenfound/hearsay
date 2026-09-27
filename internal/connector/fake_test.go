package connector_test

import (
	"context"
	"errors"
	"sync/atomic"
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

	// A factory establishes what the source needs before New can reject the
	// connector, so a rejected connector may hold a socket and the goroutine
	// reading it. New is the only thing holding it, so it has to close it.
	t.Run("closes a connector that cannot ingest", func(t *testing.T) {
		reg := connector.NewRegistry()
		c := &inert{}
		if err := reg.Register("inert", func(context.Context, connector.SourceConfig) (connector.Connector, error) {
			return c, nil
		}); err != nil {
			t.Fatalf("Register() = %v, want no error", err)
		}
		src := fakeSource()
		src.Type = "inert"
		if _, err := reg.New(t.Context(), src); !errors.Is(err, connector.ErrNoIngestMode) {
			t.Fatalf("New() = %v, want ErrNoIngestMode", err)
		}
		if !c.closed.Load() {
			t.Error("the rejected connector was not closed")
		}
	})

	t.Run("closes a connector that describes itself as another type", func(t *testing.T) {
		reg := connector.NewRegistry()
		var built *connector.Fake
		if err := reg.Register("mislabelled", func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
			built = connector.NewFake(src)
			built.Type = connector.FakeType
			return built, nil
		}); err != nil {
			t.Fatalf("Register() = %v, want no error", err)
		}
		src := fakeSource()
		src.Type = "mislabelled"
		if _, err := reg.New(t.Context(), src); err == nil {
			t.Fatal("New() = nil error, want an error about the descriptor")
		}
		// A closed Fake refuses to ingest, which is how the test sees that it
		// was closed without reaching into it.
		if err := built.Poll(t.Context(), &connector.Recorder{}); !errors.Is(err, connector.ErrClosed) {
			t.Errorf("Poll() on the rejected connector = %v, want ErrClosed", err)
		}
	})

	t.Run("reports a rejected connector that also fails to close", func(t *testing.T) {
		reg := connector.NewRegistry()
		closeErr := errors.New("socket stuck")
		if err := reg.Register("inert", func(context.Context, connector.SourceConfig) (connector.Connector, error) {
			return &inert{closeErr: closeErr}, nil
		}); err != nil {
			t.Fatalf("Register() = %v, want no error", err)
		}
		src := fakeSource()
		src.Type = "inert"
		_, err := reg.New(t.Context(), src)
		if !errors.Is(err, connector.ErrNoIngestMode) {
			t.Errorf("New() = %v, want it to wrap ErrNoIngestMode", err)
		}
		if !errors.Is(err, closeErr) {
			t.Errorf("New() = %v, want it to carry the close failure too", err)
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

// inert implements Connector and neither ingest mode, and records the close it
// is owed.
type inert struct {
	closeErr error
	closed   atomic.Bool
}

func (*inert) Describe() connector.Descriptor { return connector.Descriptor{Type: "inert"} }
func (*inert) Health(context.Context) connector.Health {
	return connector.Health{Status: connector.HealthOK}
}

func (i *inert) Close(context.Context) error {
	i.closed.Store(true)
	return i.closeErr
}
