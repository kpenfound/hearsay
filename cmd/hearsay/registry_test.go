package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// TestConnectorRegistry pins what this binary can ingest: a type registered
// here builds (or fails on its own configuration), and anything else is an
// unknown type.
func TestConnectorRegistry(t *testing.T) {
	tests := []struct {
		name        string
		src         connector.SourceConfig
		wantUnknown bool
	}{
		{
			name: "github is registered",
			src: connector.SourceConfig{
				ID: "github", Type: "github", Containers: []string{"acme/api"},
				Secrets: map[string]string{"token": "t", "webhook_secret": "s"},
			},
		},
		{
			name: "discord is registered",
			src:  connector.SourceConfig{ID: "discord", Type: "discord", Containers: []string{"1"}, Settings: []byte(`{"guild":"2"}`), Secrets: map[string]string{"token": "t"}},
		},
		{
			name: "slack is registered",
			src:  connector.SourceConfig{ID: "slack", Type: "slack", Containers: []string{"C0PUBLIC"}, Settings: []byte(`{"team":"T0001"}`), Secrets: map[string]string{"app_token": "xapp-t", "bot_token": "xoxb-t"}},
		},
		{
			name: "drive is registered",
			src:  connector.SourceConfig{ID: "drive", Type: "drive", Containers: []string{"folder"}},
		},
		{
			name: "obsidian is registered",
			src:  connector.SourceConfig{ID: "notes", Type: "obsidian", Containers: []string{"notes"}, Settings: json.RawMessage(`{"root":"/missing-vault","owner":{"source":"notes","kind":"user","native_id":"owner"}}`)},
		},
		{
			name: "tracker is registered",
			src:  connector.SourceConfig{ID: "linear", Type: "tracker", Containers: []string{"ENG"}, Settings: json.RawMessage(`{"access":{"ENG":[{"kind":"public"}]}}`), Secrets: map[string]string{"token": "t"}},
		},
		{
			name: "agent is registered",
			src:  connector.SourceConfig{ID: "sessions", Type: "agent", Containers: []string{"*"}},
		},
		{
			name: "work is registered",
			src:  connector.SourceConfig{ID: "projects", Type: "work", Containers: []string{"*"}},
		},
		{
			name:        "anything else is not",
			src:         connector.SourceConfig{ID: "unknown", Type: "unknown", Containers: []string{"1"}},
			wantUnknown: true,
		},
	}
	// The agent session connector reads its agents' tokens from the
	// environment when it is built.
	t.Setenv("HEARSAY_TEST_SHED_TOKEN", "shed-token")
	principals := []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "HEARSAY_TEST_SHED_TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := connectorRegistry(principals, []connector.SourceConfig{tt.src}).New(t.Context(), tt.src)
			if got := errors.Is(err, connector.ErrUnknownType); got != tt.wantUnknown {
				t.Fatalf("New(%q) = %v, want unknown type %v", tt.src.Type, err, tt.wantUnknown)
			}
			if err == nil {
				if err := c.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestWorkRegistryReceivesConfiguredSources(t *testing.T) {
	t.Setenv("HEARSAY_TEST_FACTORY_TOKEN", "factory-token")
	principals := []principal.Principal{{ID: "factory", Kind: principal.KindAgent, Class: principal.ClassObserver, TokenEnv: "HEARSAY_TEST_FACTORY_TOKEN"}}
	src := connector.SourceConfig{ID: "projects", Type: "work", Containers: []string{"*"}}
	sources := []connector.SourceConfig{src, {ID: "github", Type: "github", Containers: []string{"acme/api"}}}
	c, err := connectorRegistry(principals, sources).New(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	pusher, ok := c.(connector.Pusher)
	if !ok {
		t.Fatal("work is not a pusher")
	}
	rec := &connector.Recorder{}
	h := pusher.Handler(connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src)))
	for _, tt := range []struct {
		source string
		status int
	}{{"github", 202}, {"unknown", 400}} {
		t.Run(tt.source, func(t *testing.T) {
			body := `{"kind":"project","project":"p","name":"Project","repos":[{"source":"` + tt.source + `","project":"acme/api"}],"created_at":"2026-10-14T17:00:00Z"}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/hooks/projects", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer factory-token")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, req)
			if response.Code != tt.status {
				t.Fatalf("POST = %d %s", response.Code, response.Body.String())
			}
		})
	}
}
