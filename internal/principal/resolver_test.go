package principal_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

func mapping() []principal.Principal {
	return []principal.Principal{
		{
			ID:   "kyle",
			Name: "Kyle Penfound",
			Kind: principal.KindHuman,
			Identities: []principal.Identity{
				{Source: "github", NativeID: "MDQ6VXNlcjE=", Handle: "kpenfound"},
				{Source: "discord", NativeID: "302100000000000003", Handle: "kyle"},
				{Source: "drive", Handle: "Kyle@acme.example"},
			},
		},
		{
			ID:   "robin",
			Kind: principal.KindHuman,
			Identities: []principal.Identity{
				{Source: "github", Handle: "robinok"},
				{Source: "discord", Handle: "robin"},
			},
		},
		{
			ID:    "shed",
			Kind:  principal.KindAgent,
			Class: principal.ClassWorker,
			Identities: []principal.Identity{
				{Source: "github", Handle: "shed-agent[bot]"},
			},
		},
		{
			ID:      "api-team",
			Kind:    principal.KindTeam,
			Members: []string{"kyle", "robin", "shed"},
			Identities: []principal.Identity{
				{Source: "github", NativeID: "MDQ6VGVhbTE=", Handle: "acme/api-team"},
			},
		},
		{
			ID:         "eng",
			Kind:       principal.KindTeam,
			Identities: []principal.Identity{{Source: "github", Handle: "acme/eng"}},
		},
	}
}

func newResolver(t *testing.T, principals []principal.Principal) *principal.Resolver {
	t.Helper()
	r, err := principal.NewResolver(principals)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name       string
		hint       connector.Identity
		want       principal.Status
		wantID     string
		candidates []string
	}{{
		name:   "native id",
		hint:   connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjE="},
		want:   principal.Resolved,
		wantID: "kyle",
	}, {
		name:   "native id wins over a handle nobody claims",
		hint:   connector.Identity{Source: "github", NativeID: "MDQ6VXNlcjE=", Handle: "kyle-p"},
		want:   principal.Resolved,
		wantID: "kyle",
	}, {
		name:   "handle only",
		hint:   connector.Identity{Source: "github", Kind: connector.IdentityUser, Handle: "robinok"},
		want:   principal.Resolved,
		wantID: "robin",
	}, {
		name:   "handle in another case",
		hint:   connector.Identity{Source: "github", Handle: "RobinOK"},
		want:   principal.Resolved,
		wantID: "robin",
	}, {
		name:   "handle with surrounding space",
		hint:   connector.Identity{Source: "github", Handle: "  robinok "},
		want:   principal.Resolved,
		wantID: "robin",
	}, {
		name:   "email matches a handle",
		hint:   connector.Identity{Source: "drive", Handle: "", Email: "kyle@ACME.example"},
		want:   principal.Resolved,
		wantID: "kyle",
	}, {
		name:   "bot handle",
		hint:   connector.Identity{Source: "github", Kind: connector.IdentityBot, Handle: "shed-agent[bot]"},
		want:   principal.Resolved,
		wantID: "shed",
	}, {
		name:   "every key agrees",
		hint:   connector.Identity{Source: "discord", NativeID: "302100000000000003", Handle: "kyle"},
		want:   principal.Resolved,
		wantID: "kyle",
	}, {
		name:       "keys name different principals",
		hint:       connector.Identity{Source: "discord", NativeID: "302100000000000003", Handle: "robin"},
		want:       principal.Ambiguous,
		candidates: []string{"kyle", "robin"},
	}, {
		name:       "handle and email name different principals",
		hint:       connector.Identity{Source: "discord", Handle: "kyle", Email: "robin"},
		want:       principal.Ambiguous,
		candidates: []string{"kyle", "robin"},
	}, {
		name: "nobody is mapped to it",
		hint: connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjk5", Handle: "stranger"},
		want: principal.Unknown,
	}, {
		name: "right handle, wrong source",
		hint: connector.Identity{Source: "discord", Handle: "robinok"},
		want: principal.Unknown,
	}, {
		name: "no source",
		hint: connector.Identity{Handle: "robinok"},
		want: principal.Unknown,
	}, {
		name: "no keys at all",
		hint: connector.Identity{Source: "github", Kind: connector.IdentityUser},
		want: principal.Unknown,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newResolver(t, mapping()).Resolve(tt.hint)
			if got.Status != tt.want {
				t.Fatalf("Resolve(%+v).Status = %s, want %s", tt.hint, got.Status, tt.want)
			}
			switch tt.want {
			case principal.Resolved:
				if got.Principal.ID != tt.wantID {
					t.Errorf("resolved to %q, want %q", got.Principal.ID, tt.wantID)
				}
				if got.Principal.Kind == "" || len(got.Principal.Identities) == 0 {
					t.Errorf("resolved principal = %+v, want the configured entry", got.Principal)
				}
				if !slices.Equal(got.Candidates, []string{tt.wantID}) {
					t.Errorf("Candidates = %v, want just %q", got.Candidates, tt.wantID)
				}
			case principal.Ambiguous:
				if !slices.Equal(got.Candidates, tt.candidates) {
					t.Errorf("Candidates = %v, want %v", got.Candidates, tt.candidates)
				}
				if got.Principal.ID != "" {
					t.Errorf("Principal = %+v, want the zero value on an ambiguous hint", got.Principal)
				}
			case principal.Unknown:
				if got.Principal.ID != "" || len(got.Candidates) != 0 {
					t.Errorf("Resolve = %+v, want nothing", got)
				}
			}
		})
	}
}

func TestResolveGroup(t *testing.T) {
	tests := []struct {
		name   string
		group  string
		want   principal.Status
		wantID string
	}{
		{"a node id", "MDQ6VGVhbTE=", principal.Resolved, "api-team"},
		{"the slug of the same team", "acme/api-team", principal.Resolved, "api-team"},
		{"a slug is all a CODEOWNERS team has", "acme/eng", principal.Resolved, "eng"},
		{"a slug in another case", "ACME/Eng", principal.Resolved, "eng"},
		{"a group nobody claims", "MDQ6VGVhbTk5", principal.Unknown, ""},
		{"a group that is really a person", "kpenfound", principal.Resolved, "kyle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newResolver(t, mapping()).ResolveGroup("github", tt.group)
			if got.Status != tt.want || got.Principal.ID != tt.wantID {
				t.Errorf("ResolveGroup(%q) = %+v, want %s %q", tt.group, got, tt.want, tt.wantID)
			}
		})
	}

	r := newResolver(t, mapping())
	for _, blank := range [][2]string{{"github", ""}, {"github", "   "}, {"", "acme/eng"}} {
		if got := r.ResolveGroup(blank[0], blank[1]); got.Status != principal.Unknown {
			t.Errorf("ResolveGroup(%q, %q) = %+v, want unknown", blank[0], blank[1], got)
		}
	}
}

func TestNewResolverRejects(t *testing.T) {
	claimed := func(a, b principal.Identity) []principal.Principal {
		return []principal.Principal{
			{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{a}},
			{ID: "robin", Kind: principal.KindHuman, Identities: []principal.Identity{b}},
		}
	}
	tests := []struct {
		name       string
		principals []principal.Principal
		wantErr    error
		want       string
	}{{
		name:       "one native id, two principals",
		principals: claimed(principal.Identity{Source: "github", NativeID: "N1"}, principal.Identity{Source: "github", NativeID: "N1"}),
		wantErr:    principal.ErrIdentityClaimed,
	}, {
		name:       "one handle, two principals",
		principals: claimed(principal.Identity{Source: "github", Handle: "kp"}, principal.Identity{Source: "github", Handle: "kp"}),
		wantErr:    principal.ErrIdentityClaimed,
	}, {
		name:       "one handle in two cases",
		principals: claimed(principal.Identity{Source: "github", Handle: "KP"}, principal.Identity{Source: "github", Handle: "kp"}),
		wantErr:    principal.ErrIdentityClaimed,
	}, {
		name:       "duplicate principal id",
		principals: []principal.Principal{{ID: "kyle"}, {ID: "kyle"}},
		want:       "defined twice",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := principal.NewResolver(tt.principals)
			if err == nil {
				t.Fatal("NewResolver succeeded, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}

	ok := []principal.Principal{{ID: "kyle", Identities: []principal.Identity{
		{Source: "github", NativeID: "kp", Handle: "kp"},
		{Source: "discord", NativeID: "kp"},
	}}}
	if _, err := principal.NewResolver(ok); err != nil {
		t.Errorf("NewResolver: %v", err)
	}
}

func TestNewResolverSkipsEmptyKeys(t *testing.T) {
	blank := []principal.Identity{{Source: "", Handle: "kpenfound"}, {Source: "discord", Handle: " "}}
	if _, err := principal.NewResolver([]principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Identities: blank},
		{ID: "robin", Kind: principal.KindHuman, Identities: blank},
	}); err != nil {
		t.Errorf("NewResolver over two principals with the same unusable identities: %v", err)
	}

	r := newResolver(t, []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{
			{Source: "github", Handle: "kpenfound"},
			{Source: ""},
			{Source: "discord", Handle: "  "},
		}},
	})
	for _, hint := range []connector.Identity{
		{Source: "discord"},
		{Source: "discord", Handle: " "},
		{Handle: "kpenfound"},
	} {
		if got := r.Resolve(hint); got.Status != principal.Unknown {
			t.Errorf("Resolve(%+v) = %+v, want unknown", hint, got)
		}
	}
	if got := r.Resolve(connector.Identity{Source: "github", Handle: "kpenfound"}); got.Status != principal.Resolved {
		t.Errorf("Resolve(a good identity) = %+v, want resolved", got)
	}
}

func TestClaims(t *testing.T) {
	r := newResolver(t, []principal.Principal{{
		ID:   "kyle",
		Kind: principal.KindHuman,
		Identities: []principal.Identity{
			{Source: "github", NativeID: "u1", Handle: "kpenfound"},
			{Source: "discord", NativeID: "kpenfound"},
		},
	}, {
		ID:         "robin",
		Kind:       principal.KindHuman,
		Identities: []principal.Identity{{Source: "github", NativeID: "u2", Handle: "U1"}},
	}, {
		ID:         "api-team",
		Kind:       principal.KindTeam,
		Members:    []string{"kyle"},
		Identities: []principal.Identity{{Source: "github", NativeID: "t1", Handle: "acme/api-team"}},
	}})

	tests := []struct {
		name     string
		source   string
		spelling string
		want     string
	}{
		{"a native id", "github", "u2", "robin"},
		{"a handle", "github", "kpenfound", "kyle"},
		{"a handle in any case", "github", "KPenfound", "kyle"},
		{"a group by its name", "github", "acme/api-team", "api-team"},
		{"a group by its id", "github", "t1", "api-team"},
		{"a spelling only one namespace holds", "github", "U1", "robin"},
		{"one principal's native id is another's handle, folded", "github", "u1", ""},
		{"both namespaces, one principal", "discord", "kpenfound", "kyle"},
		{"a spelling nobody configured", "github", "nobody", ""},
		{"the right spelling in the wrong source", "discord", "u1", ""},
		{"no source", "", "u1", ""},
		{"no spelling", "github", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := r.Claims(tt.source, tt.spelling)
			if tt.want == "" {
				if ok {
					t.Errorf("Claims(%q, %q) = %q, true, want nobody", tt.source, tt.spelling, got)
				}
				return
			}
			if !ok || got != tt.want {
				t.Errorf("Claims(%q, %q) = %q, %v, want %q", tt.source, tt.spelling, got, ok, tt.want)
			}
		})
	}
}

func TestTeams(t *testing.T) {
	r := newResolver(t, append(mapping(),
		principal.Principal{ID: "infra-team", Kind: principal.KindTeam, Members: []string{"kyle"}},
		principal.Principal{ID: "zz-team", Kind: principal.KindTeam, Members: []string{"kyle"}},
		principal.Principal{ID: "empty-team", Kind: principal.KindTeam},
		principal.Principal{ID: "not-a-team", Kind: principal.KindHuman, Members: []string{"kyle"}},
	))
	tests := []struct {
		name string
		id   string
		want []string
	}{
		{"every team that lists them, sorted", "kyle", []string{"api-team", "infra-team", "zz-team"}},
		{"one team", "robin", []string{"api-team"}},
		{"an agent is a member like anybody else", "shed", []string{"api-team"}},
		{"a team is in no team, because a team may not contain one", "api-team", nil},
		{"nobody is in nothing", "nobody", nil},
		{"no id at all", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ids(r.Teams(tt.id)); !slices.Equal(got, tt.want) {
				t.Errorf("Teams(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}

	first := ids(r.Teams("kyle"))
	for range 5 {
		if got := ids(r.Teams("kyle")); !slices.Equal(got, first) {
			t.Fatalf("Teams(kyle) = %v and then %v: the same question answered two ways", first, got)
		}
	}
}

func TestMembers(t *testing.T) {
	r := newResolver(t, append(mapping(), principal.Principal{
		ID: "empty-team", Kind: principal.KindTeam, Members: []string{"gone"},
	}))

	if got := ids(r.Members("api-team")); !slices.Equal(got, []string{"kyle", "robin", "shed"}) {
		t.Errorf("Members(api-team) = %v", got)
	}
	if got := r.Members("empty-team"); len(got) != 0 {
		t.Errorf("Members(empty-team) = %+v, want none", got)
	}
	if got := r.Members("kyle"); got != nil {
		t.Errorf("Members(kyle) = %+v, want nil", got)
	}
	if got := r.Members("nobody"); got != nil {
		t.Errorf("Members(nobody) = %+v, want nil", got)
	}
}

func ids(ps []principal.Principal) []string {
	if len(ps) == 0 {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func TestWorkIdentitiesUsePrincipalIDs(t *testing.T) {
	r, err := principal.NewResolver([]principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman},
		{ID: "factory", Kind: principal.KindAgent},
		{ID: "mapped", Kind: principal.KindAgent, Identities: []principal.Identity{{Source: "sessions", NativeID: "mapped"}}},
	}, connector.SourceConfig{ID: "projects", Type: "work"},
		connector.SourceConfig{ID: "tasks", Type: "work"},
		connector.SourceConfig{ID: "sessions", Type: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, source, native, handle, want string
	}{
		{"human without mapping", "projects", "kyle", "", "kyle"},
		{"agent without mapping", "projects", "factory", "", "factory"},
		{"another work source", "tasks", "factory", "", "factory"},
		{"unknown principal", "projects", "stranger", "", ""},
		{"principal ids are exact", "projects", "Kyle", "", ""},
		{"a handle is not a principal id", "projects", "", "kyle", ""},
		{"unconfigured source", "elsewhere", "kyle", "", ""},
		{"agent sessions still need mapping", "sessions", "factory", "", ""},
		{"explicit agent session mapping", "sessions", "mapped", "", "mapped"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := r.Resolve(connector.Identity{Source: tt.source, NativeID: tt.native, Handle: tt.handle})
			if tt.want == "" {
				if got.Status != principal.Unknown {
					t.Fatalf("Resolve() = %+v, want unknown", got)
				}
			} else if got.Status != principal.Resolved || got.Principal.ID != tt.want {
				t.Fatalf("Resolve() = %+v, want %s", got, tt.want)
			}
		})
	}
}

func TestWorkIdentityCannotBeClaimedByAnotherPrincipal(t *testing.T) {
	_, err := principal.NewResolver([]principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman},
		{ID: "mallory", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: "projects", NativeID: "kyle"}}},
	}, connector.SourceConfig{ID: "projects", Type: "work"})
	if !errors.Is(err, principal.ErrIdentityClaimed) {
		t.Fatalf("NewResolver() = %v, want conflicting identity refused", err)
	}
}
