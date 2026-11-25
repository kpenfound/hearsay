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

type slashCase struct {
	name, text, thread                               string
	result                                           connector.CommandResult
	wantVerb, wantFrom, wantInto, wantTarget, answer string
}

var slashCases = []slashCase{
	{"pin", "pin https://slack.com/archives/C0PUBLIC/p1758700000000100", "", connector.CommandResult{Outcome: connector.CommandPinned, Scope: "code:api", Gesture: 7}, "pin", "", "", public + "/1758700000.000100", "Pinned"},
	{"merge refusal", "merge topic:vault topic:lock", "", connector.CommandResult{Outcome: connector.CommandNoSuchTopic, Topic: "topic:vault"}, "merge", "topic:vault", "topic:lock", "", "you can read"},
	{"thread field", "pin", "1758700000.000100", connector.CommandResult{Outcome: connector.CommandNotAllowed, Reason: "not ratified"}, "pin", "", "", public + "/1758700000.000100", "not ratified"},
}

type slashRun struct {
	acked   chan time.Duration
	applier *commandApplier
	answers chan map[string]string
	release chan struct{}
}

func startSlash(t *testing.T, tc slashCase) *slashRun {
	t.Helper()
	run := &slashRun{
		acked:   make(chan time.Duration, 1),
		answers: make(chan map[string]string, 1),
		release: make(chan struct{}),
	}
	response := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/answer" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected response request %s %s", r.Method, r.URL)
		}
		var got map[string]string
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		run.answers <- got
	}))
	t.Cleanup(response.Close)
	fake := newFakeSlack(t, func(_ int, ws *websocket.Conn) {
		_ = ws.WriteJSON(map[string]any{"type": "hello"})
		start := time.Now()
		_ = ws.WriteJSON(slashFixture(t, tc.text, tc.thread, response.URL+"/answer"))
		var ack map[string]any
		_ = ws.SetReadDeadline(time.Now().Add(time.Second))
		if err := ws.ReadJSON(&ack); err != nil || ack["envelope_id"] != "env-1" {
			t.Errorf("command ack = %v, %v", ack, err)
		}
		run.acked <- time.Since(start)
		_, _, _ = ws.ReadMessage()
	})
	run.applier = &commandApplier{called: make(chan connector.CommandRequest, 1), release: run.release, result: tc.result}
	startRuntime(t, fake.source(false), func(o *connector.RuntimeOptions) { o.Commands = run.applier })
	t.Cleanup(func() {
		select {
		case <-run.release:
		default:
			close(run.release)
		}
	})
	return run
}

func (r *slashRun) request(t *testing.T) connector.CommandRequest {
	t.Helper()
	select {
	case req := <-r.applier.called:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("command not applied")
	}
	return connector.CommandRequest{}
}

func TestSlashCommandIsAckedBeforeApplication(t *testing.T) {
	for _, tc := range slashCases {
		t.Run(tc.name, func(t *testing.T) {
			run := startSlash(t, tc)
			select {
			case elapsed := <-run.acked:
				if elapsed > 500*time.Millisecond {
					t.Errorf("ack took %v", elapsed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no command ack")
			}
			run.request(t)
			select {
			case got := <-run.answers:
				t.Errorf("answered before application: %v", got)
			default:
			}
		})
	}
}

func TestSlashCommandRequestNamesInvokerVerbAndTarget(t *testing.T) {
	for _, tc := range slashCases {
		t.Run(tc.name, func(t *testing.T) {
			req := startSlash(t, tc).request(t)
			if req.Source != "slack-acme" || req.Invoker.NativeID != "U0SAM" || req.Event != connector.EventID("slack-acme", "command:1758700200.123.abc") ||
				req.Verb != tc.wantVerb || req.From != tc.wantFrom || req.Into != tc.wantInto || req.Target != tc.wantTarget {
				t.Errorf("command request = %+v", req)
			}
		})
	}
}

func TestSlashCommandIsAnsweredEphemerally(t *testing.T) {
	for _, tc := range slashCases {
		t.Run(tc.name, func(t *testing.T) {
			run := startSlash(t, tc)
			run.request(t)
			close(run.release)
			select {
			case got := <-run.answers:
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
	t.Cleanup(response.Close)
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
	rt := startRuntime(t, fake.source(true), func(*connector.RuntimeOptions) {})
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
	if got := fmt.Sprint(rt.Health(t.Context())); strings.Contains(got, "failed") {
		t.Errorf("health = %s", got)
	}
}

func TestSlashCommandWithAForeignResponseURLIsNotApplied(t *testing.T) {
	response := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(response.Close)
	release := make(chan struct{})
	close(release)
	applier := &commandApplier{called: make(chan connector.CommandRequest, 8), release: release, result: connector.CommandResult{Outcome: connector.CommandPinned, Scope: "code:api", Gesture: 7}}
	foreign := []string{
		"https://evil.example/answer",
		"http://hooks.slack.com/answer",
		"https://hooks.slack.com@evil.example/answer",
		"https://hooks.slack.com/answer?token=1",
	}
	fake := newFakeSlack(t, func(_ int, ws *websocket.Conn) {
		_ = ws.WriteJSON(map[string]any{"type": "hello"})
		for i, target := range foreign {
			frame := slashFixture(t, "pin", "1758700000.000100", target)
			frame["envelope_id"] = fmt.Sprintf("foreign-%d", i)
			frame["payload"].(map[string]any)["trigger_id"] = fmt.Sprintf("foreign-%d", i)
			_ = ws.WriteJSON(frame)
		}
		_ = ws.WriteJSON(slashFixture(t, "pin", "1758700000.000100", response.URL+"/answer"))
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	})
	startRuntime(t, fake.source(false), func(o *connector.RuntimeOptions) { o.Commands = applier })
	select {
	case req := <-applier.called:
		if want := connector.EventID("slack-acme", "command:1758700200.123.abc"); req.Event != want {
			t.Errorf("first applied command = %s, want the one with a Slack response_url (%s)", req.Event, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no command was applied")
	}
}
