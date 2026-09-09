package principal_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/principal"
)

func TestEntityID(t *testing.T) {
	tests := []struct {
		name string
		in   principal.Principal
		want string
	}{
		{"a person", principal.Principal{ID: "kyle", Kind: principal.KindHuman}, "person:kyle"},
		{"an agent", principal.Principal{ID: "shed", Kind: principal.KindAgent}, "agent:shed"},
		{"a team", principal.Principal{ID: "api-team", Kind: principal.KindTeam}, "team:api-team"},
		// An id that cannot say what it names is worse than no id.
		{"no kind", principal.Principal{ID: "kyle"}, ""},
		{"a kind that does not exist", principal.Principal{ID: "kyle", Kind: "robot"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.EntityID(); got != tt.want {
				t.Errorf("EntityID() = %q, want %q", got, tt.want)
			}
		})
	}
	// Each kind has its own namespace, so two principals of different kinds
	// sharing an id are still two entities.
	seen := map[string]bool{}
	for _, k := range principal.Kinds() {
		id := principal.Principal{ID: "x", Kind: k}.EntityID()
		if id == "" || seen[id] {
			t.Errorf("%s mints %q, which is empty or already taken", k, id)
		}
		seen[id] = true
	}
}

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
