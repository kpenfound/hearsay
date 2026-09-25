package slack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/slack"
)

type commandApplier struct {
	called  chan connector.CommandRequest
	release <-chan struct{}
	result  connector.CommandResult
}

func slashFixture(t *testing.T, text, thread, response string) map[string]any {
	t.Helper()
	frame := frames(t, "slash")[0]
	payload := frame["payload"].(map[string]any)
	payload["text"] = text
	payload["response_url"] = response
	if thread != "" {
		payload["thread_ts"] = thread
	}
	return frame
}

func (a *commandApplier) Apply(_ context.Context, req connector.CommandRequest) connector.CommandResult {
	a.called <- req
	<-a.release
	return a.result
}

// Slack's response_url is a webhook for a private response. The fake has no
// channel-writing endpoint, and application waits until after the socket ack.
func TestSlashCommandIsAckedBeforeApplicationAndAnsweredEphemerally(t *testing.T) {
	for _, tc := range []struct {
		name, text, thread                               string
		result                                           connector.CommandResult
		wantVerb, wantFrom, wantInto, wantTarget, answer string
	}{
		{"pin", "pin https://slack.com/archives/C0PUBLIC/p1758700000000100", "", connector.CommandResult{Outcome: connector.CommandPinned, Scope: "code:api", Gesture: 7}, "pin", "", "", public + "/1758700000.000100", "Pinned"},
		{"merge refusal", "merge topic:vault topic:lock", "", connector.CommandResult{Outcome: connector.CommandNoSuchTopic, Topic: "topic:vault"}, "merge", "topic:vault", "topic:lock", "", "you can read"},
		{"thread field", "pin", "1758700000.000100", connector.CommandResult{Outcome: connector.CommandNotAllowed, Reason: "not ratified"}, "pin", "", "", public + "/1758700000.000100", "not ratified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := make(chan map[string]string, 1)
			response := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/answer" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected response request %s %s", r.Method, r.URL)
				}
				var got map[string]string
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				answers <- got
			}))
			defer response.Close()
			acked := make(chan time.Duration, 1)
			fake := newFakeSlack(t, func(_ int, ws *websocket.Conn) {
				_ = ws.WriteJSON(map[string]any{"type": "hello"})
				start := time.Now()
				_ = ws.WriteJSON(slashFixture(t, tc.text, tc.thread, response.URL+"/answer"))
				var ack map[string]any
				_ = ws.SetReadDeadline(time.Now().Add(time.Second))
				if err := ws.ReadJSON(&ack); err != nil || ack["envelope_id"] != "env-1" {
					t.Errorf("command ack = %v, %v", ack, err)
				}
				acked <- time.Since(start)
				_, _, _ = ws.ReadMessage()
			})
			reg := connector.NewRegistry()
			if err := reg.Register(slack.Type, slack.Factory); err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			applier := &commandApplier{called: make(chan connector.CommandRequest, 1), release: release, result: tc.result}
			rt, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{fake.source(false)}, Registry: reg,
				Sink: &connector.Recorder{}, Lookup: lookup, Commands: applier, Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: time.Millisecond, Shutdown: time.Second}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- rt.Run(ctx) }()
			defer func() { cancel(); <-done }()
			select {
			case elapsed := <-acked:
				if elapsed > 500*time.Millisecond {
					t.Errorf("ack took %v", elapsed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no command ack")
			}
			select {
			case req := <-applier.called:
				if req.Source != "slack-acme" || req.Invoker.NativeID != "U0SAM" || req.Event != connector.EventID("slack-acme", "command:1758700200.123.abc") ||
					req.Verb != tc.wantVerb || req.From != tc.wantFrom || req.Into != tc.wantInto || req.Target != tc.wantTarget {
					t.Errorf("command request = %+v", req)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("command not applied")
			}
			select {
			case got := <-answers:
				t.Errorf("answered before application: %v", got)
			default:
			}
			close(release)
			select {
			case got := <-answers:
				if got["response_type"] != "ephemeral" || !strings.Contains(got["text"], tc.answer) {
					t.Errorf("answer = %v", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no ephemeral answer")
			}
		})
	}
}

func TestReadOnlyIgnoresSlashCommands(t *testing.T) {
	requests := make(chan struct{}, 1)
	response := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests <- struct{}{} }))
	defer response.Close()
	acked := make(chan struct{}, 1)
	fake := newFakeSlack(t, func(_ int, ws *websocket.Conn) {
		_ = ws.WriteJSON(map[string]any{"type": "hello"})
		frame := slashFixture(t, "merge topic:a topic:b", "", response.URL)
		frame["envelope_id"] = "read-only"
		_ = ws.WriteJSON(frame)
		var ack map[string]any
		if err := ws.ReadJSON(&ack); err != nil || ack["envelope_id"] != "read-only" {
			t.Errorf("ack = %v, %v", ack, err)
		}
		acked <- struct{}{}
		_, _, _ = ws.ReadMessage()
	})
	reg := connector.NewRegistry()
	if err := reg.Register(slack.Type, slack.Factory); err != nil {
		t.Fatal(err)
	}
	rt, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{fake.source(true)}, Registry: reg, Sink: &connector.Recorder{}, Lookup: lookup,
		Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: time.Millisecond, Shutdown: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-acked:
	case <-time.After(2 * time.Second):
		t.Fatal("no ack")
	}
	select {
	case <-requests:
		t.Fatal("read-only command answered")
	case <-time.After(50 * time.Millisecond):
	}
	if got := fmt.Sprint(rt.Health(ctx)); strings.Contains(got, "failed") {
		t.Errorf("health = %s", got)
	}
}
