package main

import (
	"encoding/json"
	"errors"
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
			c, err := connectorRegistry(principals).New(t.Context(), tt.src)
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
