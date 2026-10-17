package push_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
	"github.com/kpenfound/hearsay/internal/l0"
)

func TestHandler(t *testing.T) {
	tests := []struct {
		name, method, token, body string
		result                    push.Result
		err                       error
		want                      int
		process                   bool
		contains                  string
	}{
		{name: "accepted", result: push.Result{Event: connector.Event{ID: "evt:projects:p@r"}}, want: 202, process: true, contains: `{"id":"evt:projects:p@r"}`},
		{name: "absent deletion", result: push.Result{NoContent: true}, want: 204, process: true},
		{name: "invalid request", err: fmt.Errorf("invalid: %w", push.ErrBadRequest), want: 400, process: true, contains: "invalid"},
		{name: "gate rejects event", err: fmt.Errorf("gate: %w", connector.ErrInvalidEvent), want: 400, process: true},
		{name: "missing credential", token: "-", want: 401},
		{name: "unknown credential", token: "unknown", want: 401},
		{name: "forbidden", err: push.Refuse(push.ErrForbidden, "not an attributor"), want: 403, process: true, contains: "not an attributor"},
		{name: "method before auth", method: http.MethodGet, token: "-", want: 405},
		{name: "missing project", err: fmt.Errorf("project not held: %w", push.ErrConflict), want: 409, process: true, contains: "project not held"},
		{name: "rewrite", result: push.Result{Event: connector.Event{NativeID: "p@r"}}, err: fmt.Errorf("emit: %w", l0.ErrRewrite), want: 409, process: true, contains: "p@r already holds a different event"},
		{name: "emission failure", err: errors.New("storage unavailable"), want: 500, process: true, contains: "could not be ingested"},
		{name: "propagation failure after parent emitted", result: push.Result{Event: connector.Event{ID: "evt:projects:p@r"}}, err: errors.New("child emit failed"), want: 500, process: true},
		{name: "failure overrides no content", result: push.Result{NoContent: true}, err: errors.New("read failed"), want: 500, process: true},
		{name: "empty", body: " ", want: 400},
		{name: "not JSON", body: "garbage", want: 400},
		{name: "unknown field", body: `{"unknown":true}`, want: 400},
		{name: "two objects", body: `{} {}`, want: 400},
		{name: "trailing scalar", body: `{} 1`, want: 400},
		{name: "trailing junk", body: `{} garbage`, want: 400},
		{name: "unmatched close", body: `{} }`, want: 400},
		{name: "null", body: `null`, want: 400},
		{name: "array", body: `[]`, want: 400},
		{name: "oversize", body: strings.Repeat(" ", push.MaxRequest+1), want: 413},
		{name: "exact limit", body: `{}` + strings.Repeat(" ", push.MaxRequest-2), want: 202, process: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type request struct {
				Text string `json:"text"`
			}
			calls, observed := 0, 0
			handler := push.Handler(push.Options[request]{
				Authenticate: func(_ context.Context, token string) (string, bool) { return "poster", token == "secret" },
				Process: func(_ context.Context, poster string, _ request) (push.Result, error) {
					calls++
					if poster != "poster" {
						t.Fatalf("Process(poster=%q), want authenticated poster", poster)
					}
					return tt.result, tt.err
				},
				Observe: func(_ context.Context, _ string, _ request, result push.Result, err error) {
					observed++
					if result.NoContent != tt.result.NoContent || !errors.Is(err, tt.err) {
						t.Fatalf("Observe() = %+v, %v, want original work result", result, err)
					}
				},
			})
			method, body, token := tt.method, tt.body, tt.token
			if method == "" {
				method = http.MethodPost
			}
			if body == "" {
				body = `{"text":"update"}`
			}
			if token == "" {
				token = "secret"
			}
			req := httptest.NewRequestWithContext(t.Context(), method, "/", strings.NewReader(body))
			if token != "-" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.want || !strings.Contains(w.Body.String(), tt.contains) {
				t.Fatalf("Handler() = %d %s, want %d containing %q", w.Code, w.Body, tt.want, tt.contains)
			}
			if (calls == 1) != tt.process || observed != calls {
				t.Fatalf("Process/Observe called %d/%d times, want work %v", calls, observed, tt.process)
			}
			switch w.Code {
			case http.StatusAccepted:
				var accepted push.Accepted
				if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil || accepted.ID != tt.result.Event.ID {
					t.Fatalf("accepted = %+v, %v, want id %q", accepted, err, tt.result.Event.ID)
				}
				if w.Header().Get("Content-Type") != "application/json" {
					t.Fatal("accepted response is not application/json")
				}
			case http.StatusNoContent:
				if w.Body.Len() != 0 {
					t.Fatal("204 has a body")
				}
			case http.StatusUnauthorized:
				if w.Header().Get("WWW-Authenticate") != `Bearer realm="hearsay"` {
					t.Fatal("401 is missing the bearer challenge")
				}
			}
		})
	}
}

type unreadableBody struct{ reads int }

func (b *unreadableBody) Read([]byte) (int, error) {
	b.reads++
	return 0, errors.New("body cannot be read")
}
func (*unreadableBody) Close() error { return nil }

func TestHandlerAuthenticationBeforeBody(t *testing.T) {
	tests := []struct {
		name, token string
		want, reads int
	}{
		{"unknown", "unknown", 401, 0}, {"missing", "", 401, 0}, {"authenticated read failure", "secret", 413, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &unreadableBody{}
			handler := push.Handler(push.Options[struct{}]{
				Authenticate: func(_ context.Context, token string) (string, bool) { return "poster", token == "secret" },
				Process: func(context.Context, string, struct{}) (push.Result, error) {
					t.Fatal("Process called without a readable body")
					return push.Result{}, nil
				},
			})
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", body)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.want || body.reads != tt.reads {
				t.Fatalf("Handler() = %d, %d reads, want %d, %d reads", w.Code, body.reads, tt.want, tt.reads)
			}
		})
	}
}

func TestHandlerWaitsForWork(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := push.Handler(push.Options[struct{}]{
		Authenticate: func(context.Context, string) (string, bool) { return "poster", true },
		Process: func(context.Context, string, struct{}) (push.Result, error) {
			close(started)
			<-release
			return push.Result{}, errors.New("propagation failed")
		},
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	go func() { defer close(done); handler.ServeHTTP(w, req) }()
	<-started
	// Work has started but cannot finish: no accepted response is written yet.
	if w.Flushed || w.Body.Len() != 0 {
		t.Error("handler wrote before work completed")
	}
	close(release)
	<-done
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("Handler() = %d, want 500 after propagation failure", w.Code)
	}
}

var _ io.ReadCloser = (*unreadableBody)(nil)
