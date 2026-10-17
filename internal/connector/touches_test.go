package connector_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
)

func TestEventTouches(t *testing.T) {
	touch := connector.Touch{Source: "github-acme", Project: "acme/api"}
	withPaths := func(n int) connector.Touch {
		out := touch
		out.Paths = slices.Repeat([]string{"internal/api.go"}, n)
		return out
	}
	for _, tt := range []struct {
		name    string
		touches []connector.Touch
		want    string
	}{
		{"absent", nil, ""},
		{"empty", []connector.Touch{}, ""},
		{"repository only", []connector.Touch{touch}, ""},
		{"paths", []connector.Touch{withPaths(2)}, ""},
		{"maximum entries", slices.Repeat([]connector.Touch{touch}, 16), ""},
		{"maximum paths", []connector.Touch{withPaths(300)}, ""},
		{"maximum paths across entries", []connector.Touch{withPaths(150), withPaths(150)}, ""},
		{"too many entries", slices.Repeat([]connector.Touch{touch}, 17), "limit is 16"},
		{"too many paths", []connector.Touch{withPaths(301)}, "300 paths in total"},
		{"too many total paths", []connector.Touch{withPaths(150), withPaths(151)}, "300 paths in total"},
		{"invalid source", []connector.Touch{{Source: "GitHub", Project: "acme/api"}}, "source"},
		{"missing source", []connector.Touch{{Project: "acme/api"}}, "source"},
		{"long source", []connector.Touch{{Source: strings.Repeat("a", 65), Project: "acme/api"}}, "source"},
		{"missing project", []connector.Touch{{Source: "github-acme"}}, "project is empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			event := validEvent()
			event.Payload.Touches = tt.touches
			err := event.Validate()
			if tt.want == "" {
				if err != nil {
					t.Errorf("Validate() = %v", err)
				}
			} else if !errors.Is(err, connector.ErrInvalidEvent) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want ErrInvalidEvent containing %q", err, tt.want)
			}
		})
	}
}

func TestTouchesOptionalJSON(t *testing.T) {
	for _, tt := range []struct {
		name    string
		touches []connector.Touch
		want    string
	}{
		{"absent", nil, ""},
		{"empty", []connector.Touch{}, ""},
		{"without paths", []connector.Touch{{Source: "github", Project: "acme/api"}}, `"touches":[{"source":"github","project":"acme/api"}]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(connector.Payload{Touches: tt.touches})
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if strings.Contains(string(encoded), `"touches"`) {
					t.Errorf("empty touches present in %s", encoded)
				}
			} else if !strings.Contains(string(encoded), tt.want) {
				t.Errorf("JSON = %s, want %s", encoded, tt.want)
			}
		})
	}
}
