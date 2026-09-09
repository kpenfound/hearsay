package principal_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/principal"
)

func TestKindValid(t *testing.T) {
	kinds := principal.Kinds()
	if !slices.Equal(kinds, []principal.Kind{principal.KindHuman, principal.KindAgent, principal.KindTeam}) {
		t.Fatalf("Kinds() = %v", kinds)
	}
	for _, k := range kinds {
		if !k.Valid() {
			t.Errorf("%s is not valid", k)
		}
	}
	for _, k := range []principal.Kind{"", "Human", "bot", "person"} {
		if k.Valid() {
			t.Errorf("%q is valid", k)
		}
	}
	kinds[0] = "robot"
	if principal.Kinds()[0] != principal.KindHuman {
		t.Error("Kinds() shares its slice")
	}
}

func TestFoldHandle(t *testing.T) {
	tests := []struct{ in, want string }{
		{"kpenfound", "kpenfound"},
		{"KPenfound", "kpenfound"},
		{"  Kyle@Acme.Example ", "kyle@acme.example"},
		{"shed-agent[BOT]", "shed-agent[bot]"},
		{"   ", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := principal.FoldHandle(tt.in); got != tt.want {
			t.Errorf("FoldHandle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"kyle", "api-team", "shed_2", "a", "0", strings.Repeat("a", principal.MaxIDLen)} {
		if !principal.ValidID(id) {
			t.Errorf("%q is not a principal id", id)
		}
	}
	for _, id := range []string{"", "Kyle", "kyle penfound", "-kyle", "_kyle", "kyle@acme", strings.Repeat("a", principal.MaxIDLen+1)} {
		if principal.ValidID(id) {
			t.Errorf("%q is a principal id", id)
		}
	}
}

func TestStatusString(t *testing.T) {
	tests := []struct {
		in   principal.Status
		want string
	}{
		{principal.Unknown, "unknown"},
		{principal.Resolved, "resolved"},
		{principal.Ambiguous, "ambiguous"},
		{principal.Status(9), "invalid"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Status(%d).String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}
