package main

import (
	"errors"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
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
			name: "drive is registered",
			src:  connector.SourceConfig{ID: "drive", Type: "drive", Containers: []string{"folder"}},
		},
		{
			name:        "anything else is not",
			src:         connector.SourceConfig{ID: "unknown", Type: "unknown", Containers: []string{"1"}},
			wantUnknown: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := connectorRegistry().New(t.Context(), tt.src)
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
