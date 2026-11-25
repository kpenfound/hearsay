package connector_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

func TestAllowlistAllows(t *testing.T) {
	allow := connector.NewAllowlist(
		connector.SourceConfig{ID: "github-acme", Type: "github", Containers: []string{"acme/api", "acme/web"}},
		connector.SourceConfig{ID: "discord-eng", Type: "discord", Containers: []string{"C123"}},
		connector.SourceConfig{ID: "agent-sessions", Type: "agent", Containers: []string{connector.AllowAll}},
		connector.SourceConfig{ID: "drive-team", Type: "drive"},
	)

	tests := []struct {
		name      string
		source    string
		container string
		want      bool
	}{
		{name: "a listed container", source: "github-acme", container: "acme/api", want: true},
		{name: "another listed container", source: "github-acme", container: "acme/web", want: true},
		{name: "a container of the same source that is not listed", source: "github-acme", container: "acme/secrets"},
		{name: "a container listed under another source", source: "discord-eng", container: "acme/api"},
		{name: "a source that is not configured", source: "slack-eng", container: "C123"},
		{name: "the wildcard allows any container", source: "agent-sessions", container: "session-9", want: true},
		{name: "a source with no containers allows none", source: "drive-team", container: "folder-1"},
		{name: "an empty container is never allowed", source: "agent-sessions", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allow.Allows(tt.source, tt.container); got != tt.want {
				t.Errorf("Allows(%q, %q) = %v, want %v", tt.source, tt.container, got, tt.want)
			}
		})
	}
}

func TestZeroAllowlistAllowsNothing(t *testing.T) {
	var allow connector.Allowlist
	if allow.Allows("github-acme", "acme/api") {
		t.Error("the zero Allowlist allowed an event")
	}
}

func gateFixture(t *testing.T) (*connector.Gate, *connector.Recorder) {
	t.Helper()
	src := connector.SourceConfig{ID: "github-acme", Type: "github", Containers: []string{"acme/api"}}
	rec := &connector.Recorder{}
	desc := connector.Descriptor{Type: "github", Kinds: []connector.Kind{connector.KindMessage, connector.KindIssue, connector.KindTombstone}}
	return connector.NewGate(rec, src.ID, desc, connector.NewAllowlist(src)), rec
}

func TestGateWritesAnAllowlistedEvent(t *testing.T) {
	gate, rec := gateFixture(t)
	ev := validEvent()

	if err := gate.Emit(t.Context(), ev); err != nil {
		t.Fatalf("Emit() = %v, want no error", err)
	}
	got := rec.Events()
	if len(got) != 1 {
		t.Fatalf("recorded %d events, want 1", len(got))
	}
	if want := connector.EventID(ev.Source, ev.NativeID); got[0].ID != want {
		t.Errorf("emitted id = %q, want %q", got[0].ID, want)
	}
	if gate.Dropped() != 0 {
		t.Errorf("Dropped() = %d, want 0", gate.Dropped())
	}
}

func TestGateDropsEventsOutsideTheAllowlist(t *testing.T) {
	gate, rec := gateFixture(t)
	ev := validEvent()
	ev.Payload.Container.NativeID = "acme/secrets"

	if err := gate.Emit(t.Context(), ev); err != nil {
		t.Fatalf("Emit() = %v, want no error: a drop is not the connector's fault", err)
	}
	if got := rec.Events(); len(got) != 0 {
		t.Fatalf("recorded %d events, want none: %+v", len(got), got)
	}
	if gate.Dropped() != 1 {
		t.Errorf("Dropped() = %d, want 1", gate.Dropped())
	}
}

func TestGateRejectsContractViolations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*connector.Event)
		wantErr error
	}{
		{
			name:    "an event from another source",
			mutate:  func(e *connector.Event) { e.Source = "github-other" },
			wantErr: connector.ErrForeignSource,
		},
		{
			name:    "an invalid event",
			mutate:  func(e *connector.Event) { e.ACL = nil },
			wantErr: connector.ErrInvalidEvent,
		},
		{
			name:    "a kind the connector did not declare",
			mutate:  func(e *connector.Event) { e.Kind = connector.KindCommit },
			wantErr: connector.ErrUndeclaredKind,
		},
		{
			name: "an extension kind the connector did not declare",
			mutate: func(e *connector.Event) {
				e.Kind = "github.discussion"
				e.Payload.BaseKind = connector.KindMessage
			},
			wantErr: connector.ErrUndeclaredKind,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate, rec := gateFixture(t)
			ev := validEvent()
			tt.mutate(&ev)

			err := gate.Emit(t.Context(), ev)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Emit() = %v, want an error wrapping %v", err, tt.wantErr)
			}
			if got := rec.Events(); len(got) != 0 {
				t.Errorf("recorded %d events, want none", len(got))
			}
		})
	}
}

func TestGatePassesSinkErrorsBack(t *testing.T) {
	src := connector.SourceConfig{ID: "github-acme", Containers: []string{"acme/api"}}
	wantErr := errors.New("l0 is down")
	rec := &connector.Recorder{Err: wantErr}
	gate := connector.NewGate(rec, src.ID, connector.Descriptor{Kinds: []connector.Kind{connector.KindMessage}}, connector.NewAllowlist(src))

	if err := gate.Emit(t.Context(), validEvent()); !errors.Is(err, wantErr) {
		t.Errorf("Emit() = %v, want %v", err, wantErr)
	}
}

func TestAGateDropLogsIdsAndNeverThePayload(t *testing.T) {
	var out bytes.Buffer
	ctx := telemetry.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	gate, _ := gateFixture(t)
	ev := validEvent()
	ev.Payload.Container.NativeID = "acme/secrets"
	ev.Payload.Text = "the migration password is hunter2"

	if err := gate.Emit(ctx, ev); err != nil {
		t.Fatalf("Emit() = %v, want no error", err)
	}
	logs := out.String()
	if !strings.Contains(logs, "acme/secrets") || !strings.Contains(logs, ev.NativeID) {
		t.Fatalf("the drop was logged as %q, want the container and native id", logs)
	}
	if strings.Contains(logs, "hunter2") || strings.Contains(logs, ev.Payload.Author.Handle) {
		t.Errorf("the drop log carries payload or author content: %s", logs)
	}
}
