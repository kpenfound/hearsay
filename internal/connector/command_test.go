package connector_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
)

// applier is a CommandApplier that records what it was handed and answers
// with a scripted result.
type applier struct {
	mu   sync.Mutex
	reqs []connector.CommandRequest
	res  connector.CommandResult
}

func (a *applier) Apply(_ context.Context, req connector.CommandRequest) connector.CommandResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reqs = append(a.reqs, req)
	return a.res
}

// failingSink refuses every event.
type failingSink struct{}

func (failingSink) Emit(context.Context, connector.Event) error {
	return errors.New("the database is down")
}

// deletedSink records every command as one an operator deleted.
type deletedSink struct{ connector.Recorder }

func (*deletedSink) RecordCommand(context.Context, connector.Event) (bool, error) { return true, nil }

// A connector hands a command it parsed to its sink; the runtime records it
// as an L0 `command` event, applies it through the process's applier, and
// hands back the applier's result for the connector to answer with. Every
// case in which it is not applied says why, and records nothing it should
// not.
func TestTheRuntimeRecordsAndAppliesACommand(t *testing.T) {
	answered := connector.CommandResult{Outcome: connector.CommandPinned, Principal: "kyle", Kind: "human", Scope: "code:api", Gesture: 7}
	tests := []struct {
		name     string
		readOnly bool
		kinds    []connector.Kind
		sink     func() connector.Sink
		noApply  bool
		edit     func(*connector.Command)
		// status is the fake's HTTP answer; want is the result it carries.
		status   int
		want     connector.CommandResult
		recorded bool
		applied  bool
	}{
		{name: "a command is recorded and applied", status: http.StatusOK, want: answered, recorded: true, applied: true},
		{name: "a read-only source takes none", readOnly: true, status: http.StatusNoContent},
		{name: "a command outside the allowlist is not read", edit: func(c *connector.Command) { c.Event.Payload.Container.NativeID = "C999" },
			status: http.StatusOK, want: connector.CommandResult{Outcome: connector.CommandNotRead}},
		{name: "a command that cannot be recorded is not applied", sink: func() connector.Sink { return failingSink{} },
			status: http.StatusOK, want: connector.CommandResult{Outcome: connector.CommandNotRecorded}},
		{name: "a command an operator deleted is not applied again", sink: func() connector.Sink { return &deletedSink{} },
			status: http.StatusOK, want: connector.CommandResult{Outcome: connector.CommandDeleted}},
		{name: "a runtime with no applier refuses it", noApply: true, status: http.StatusInternalServerError},
		{name: "a command is a command event", edit: func(c *connector.Command) { c.Event.Kind = connector.KindMessage },
			status: http.StatusInternalServerError},
		{name: "a connector must declare command", kinds: []connector.Kind{connector.KindMessage},
			status: http.StatusInternalServerError},
		{name: "a command is from the connector's source", edit: func(c *connector.Command) { c.Event.Source = "elsewhere" },
			status: http.StatusInternalServerError},
		{name: "a command has an author", edit: func(c *connector.Command) { c.Event.Payload.Author = nil },
			status: http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := runtimeSource("fake-chat")
			src.ReadOnly = tt.readOnly
			fake := connector.NewFake(src)
			fake.Kinds = tt.kinds
			rec := &connector.Recorder{}
			var sink connector.Sink = rec
			if tt.sink != nil {
				sink = tt.sink()
			}
			apply := &applier{res: answered}
			opts := connector.RuntimeOptions{
				Sources:  []connector.SourceConfig{src},
				Registry: registryOf(t, map[string]connector.Connector{src.ID: &pushOnly{fake: fake}}),
				Sink:     sink,
				Commands: apply,
				Cadence:  quick(),
			}
			if tt.noApply {
				opts.Commands = nil
			}
			runtime, err := connector.NewRuntime(t.Context(), opts)
			if err != nil {
				t.Fatalf("NewRuntime() = %v", err)
			}
			t.Cleanup(func() { _ = runtime.Close(context.WithoutCancel(t.Context())) })

			ev := fake.NewEvent(connector.KindCommand, "interaction:1", "/hearsay pin")
			cmd := connector.Command{Event: ev, Verb: connector.CommandPin, Target: "thread:C123/1"}
			if tt.edit != nil {
				tt.edit(&cmd)
			}
			body, err := json.Marshal(cmd)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, connector.HookPath(src.ID)+"?command", strings.NewReader(string(body)))
			w := httptest.NewRecorder()
			runtime.Handler().ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("the command was answered %d (%s), want %d", w.Code, w.Body, tt.status)
			}
			if tt.status == http.StatusOK {
				var got connector.CommandResult
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("result %+v, want %+v", got, tt.want)
				}
			}
			if tt.status == http.StatusNoContent && w.Body.Len() != 0 {
				t.Errorf("a read-only source answered %q", w.Body)
			}

			events := rec.Events()
			if tt.recorded != (len(events) == 1) || len(events) > 1 {
				t.Fatalf("recorded %d events, want recorded = %v", len(events), tt.recorded)
			}
			wantID := connector.EventID(src.ID, "interaction:1")
			if tt.recorded && (events[0].Kind != connector.KindCommand || events[0].ID != wantID) {
				t.Errorf("recorded %s %q, want a command %q", events[0].Kind, events[0].ID, wantID)
			}
			if !tt.applied {
				if len(apply.reqs) != 0 {
					t.Errorf("applied %+v, want nothing applied", apply.reqs)
				}
				return
			}
			want := connector.CommandRequest{Source: src.ID, Event: wantID, Invoker: *ev.Payload.Author, Verb: connector.CommandPin, Target: "thread:C123/1"}
			if len(apply.reqs) != 1 || !reflect.DeepEqual(apply.reqs[0], want) {
				t.Errorf("applied %+v, want %+v", apply.reqs, want)
			}
		})
	}
}

// A result applied it, now or when the same command was first applied; every
// refusal changed nothing.
func TestCommandResultApplied(t *testing.T) {
	tests := []struct {
		outcome connector.CommandOutcome
		want    bool
	}{
		{connector.CommandPinned, true},
		{connector.CommandAlreadyPinned, true},
		{connector.CommandMerged, true},
		{connector.CommandNotAllowed, false},
		{connector.CommandUnmapped, false},
		{connector.CommandNotRead, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.outcome), func(t *testing.T) {
			if got := (connector.CommandResult{Outcome: tt.outcome}).Applied(); got != tt.want {
				t.Errorf("Applied() = %v, want %v", got, tt.want)
			}
		})
	}
}
