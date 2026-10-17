package push_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
)

func observation() (connector.Event, time.Time) {
	return connector.Event{
		Source: "projects", Kind: connector.KindIssue,
		Time:    time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC),
		Payload: connector.Payload{Artifact: "p", Container: connector.Container{Kind: "project", NativeID: "p"}, Title: "Plan"},
		ACL:     connector.ACL{{Kind: connector.ACLPublic}},
	}, time.Date(2026, 9, 23, 15, 30, 0, 0, time.UTC)
}

func TestRevisionStability(t *testing.T) {
	base, edited := observation()
	revised, err := push.Revise(base, edited)
	if err != nil {
		t.Fatal(err)
	}
	// The first 16 SHA-256 hex digits of the original tracker's canonical JSON.
	const token = "0ace0b89398878c6"
	if revised.Payload.Revision.Token != token || revised.NativeID != "p@"+token || revised.ID != "evt:projects:p@"+token {
		t.Fatalf("Revise() = %+v, want the fixed original revision %s", revised, token)
	}
	tests := []struct {
		name   string
		change func(*connector.Event, *time.Time)
		same   bool
	}{
		{"identical", func(*connector.Event, *time.Time) {}, true},
		{"existing revision replaced", func(ev *connector.Event, _ *time.Time) { *ev = revised }, true},
		{"kind", func(ev *connector.Event, _ *time.Time) { ev.Kind = connector.KindDocument }, false},
		{"created time", func(ev *connector.Event, _ *time.Time) { ev.Time = ev.Time.Add(time.Second) }, false},
		{"edited time", func(_ *connector.Event, at *time.Time) { *at = at.Add(time.Second) }, false},
		{"text", func(ev *connector.Event, _ *time.Time) { ev.Payload.Text = "Changed" }, false},
		{"native", func(ev *connector.Event, _ *time.Time) { ev.Payload.Native = json.RawMessage(`{"poster":"factory"}`) }, false},
		{"access only", func(ev *connector.Event, _ *time.Time) {
			ev.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: "projects", NativeID: "factory"}}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, at := observation()
			tt.change(&ev, &at)
			got, err := push.Revise(ev, at)
			if err != nil {
				t.Fatal(err)
			}
			if (got.ID == revised.ID) != tt.same {
				t.Fatalf("Revise().ID = %q, want same as original %v", got.ID, tt.same)
			}
			if got.Payload.Revision.EditedAt != at {
				t.Fatalf("Revise().EditedAt = %v, want %v", got.Payload.Revision.EditedAt, at)
			}
		})
	}
}

func TestRevisionEncodingError(t *testing.T) {
	ev, edited := observation()
	ev.Payload.Native = json.RawMessage(`{`)
	if _, err := push.Revise(ev, edited); err == nil {
		t.Fatal("Revise(invalid native JSON) = nil, want an encoding error")
	}
}

type artifactReader struct {
	current connector.Event
	held    bool
	err     error
}

func (r artifactReader) CurrentArtifact(context.Context, string, string) (connector.Event, bool, error) {
	return r.current, r.held, r.err
}
func (r artifactReader) CurrentArtifacts(context.Context, string) ([]connector.Event, error) {
	return nil, r.err
}

func TestTombstone(t *testing.T) {
	ev, edited := observation()
	current, err := push.Revise(ev, edited)
	if err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("read unavailable")
	tests := []struct {
		name          string
		reader        connector.ArtifactReader
		held, wantErr bool
	}{
		{"held", artifactReader{current: current, held: true}, true, false},
		{"not held", artifactReader{}, false, false},
		{"read failure", artifactReader{err: readErr}, false, true},
		{"no reader", nil, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, held, err := push.Tombstone(t.Context(), tt.reader, ev.Source, ev.Payload.Artifact)
			if held != tt.held || (err != nil) != tt.wantErr {
				t.Fatalf("Tombstone() = %+v, %v, %v, want held %v, error %v", got, held, err, tt.held, tt.wantErr)
			}
			if tt.name == "read failure" && !errors.Is(err, readErr) {
				t.Fatalf("Tombstone() = %v, want wrapped read failure", err)
			}
			if !held {
				return
			}
			if got.Kind != connector.KindTombstone || got.Payload.Target != ev.Payload.Artifact || got.Time != ev.Time || got.Payload.Container != ev.Payload.Container || !reflect.DeepEqual(got.ACL, ev.ACL) || got.Payload.State != nil || got.Payload.Revision != nil {
				t.Fatalf("Tombstone() = %+v, want stateless retraction with current time, container and ACL", got)
			}
			again, _, err := push.Tombstone(t.Context(), tt.reader, ev.Source, ev.Payload.Artifact)
			if err != nil || !reflect.DeepEqual(got, again) {
				t.Fatalf("Tombstone(retry) = %+v, %v, want %+v", again, err, got)
			}
			changed := current
			changed.NativeID += "changed"
			next, _, err := push.Tombstone(t.Context(), artifactReader{current: changed, held: true}, ev.Source, ev.Payload.Artifact)
			if err != nil || next.ID == got.ID {
				t.Fatalf("Tombstone(new revision) = %+v, %v, want a different id", next, err)
			}
		})
	}
}
