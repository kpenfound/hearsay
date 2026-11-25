package principal_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/principal"
)

var (
	kyle = principal.Principal{ID: "kyle", Kind: principal.KindHuman}
	shed = principal.Principal{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker}
	team = principal.Principal{ID: "api-team", Kind: principal.KindTeam, Members: []string{"kyle"}}
)

func TestScopesIntersect(t *testing.T) {
	tests := []struct {
		name string
		a, b principal.Scopes
		want principal.Scopes
	}{{
		name: "every scope and every scope",
		a:    principal.AllScopes(),
		b:    principal.AllScopes(),
		want: principal.AllScopes(),
	}, {
		name: "every scope and a list",
		a:    principal.AllScopes(),
		b:    principal.SomeScopes("api", "infra"),
		want: principal.SomeScopes("api", "infra"),
	}, {
		name: "two lists",
		a:    principal.SomeScopes("api", "infra"),
		b:    principal.SomeScopes("infra", "web"),
		want: principal.SomeScopes("infra"),
	}, {
		name: "nothing in common",
		a:    principal.SomeScopes("api"),
		b:    principal.SomeScopes("web"),
		want: principal.Scopes{},
	}, {
		name: "the zero value and every scope",
		a:    principal.Scopes{},
		b:    principal.AllScopes(),
		want: principal.Scopes{},
	}, {
		name: "duplicates collapse and the result is sorted",
		a:    principal.SomeScopes("web", "api", "api"),
		b:    principal.SomeScopes("api", "web"),
		want: principal.SomeScopes("api", "web"),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, got := range []principal.Scopes{tt.a.Intersect(tt.b), tt.b.Intersect(tt.a)} {
				if got.All != tt.want.All || !slices.Equal(got.IDs, tt.want.IDs) {
					t.Errorf("Intersect = %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}

func TestScopesHasAndEmpty(t *testing.T) {
	all, some, none := principal.AllScopes(), principal.SomeScopes("api"), principal.Scopes{}
	for _, tt := range []struct {
		name  string
		in    principal.Scopes
		has   bool
		empty bool
	}{
		{"every scope has one that does not exist yet", all, true, false},
		{"a list has what it lists", some, true, false},
		{"the zero value has nothing", none, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Has("api"); got != tt.has {
				t.Errorf("Has(api) = %v, want %v", got, tt.has)
			}
			if got := tt.in.Empty(); got != tt.empty {
				t.Errorf("Empty() = %v, want %v", got, tt.empty)
			}
		})
	}
	if some.Has("web") {
		t.Error("a list of api has web")
	}
	ids := []string{"api"}
	s := principal.SomeScopes(ids...)
	ids[0] = "everything"
	if !s.Has("api") {
		t.Error("SomeScopes kept the caller's slice")
	}
}

func TestHumanRead(t *testing.T) {
	grant := principal.Grant{
		Scopes: principal.SomeScopes("api"),
		Rights: principal.Rights{Read: principal.ReadAll, Write: principal.WriteRatify},
	}
	got, err := principal.HumanRead(kyle, grant)
	if err != nil {
		t.Fatalf("HumanRead: %v", err)
	}
	if got.Human != "kyle" || got.Agent != "" {
		t.Errorf("Effective = %+v, want kyle reading directly", got)
	}
	if !got.Grant.Rights.Write.Has(principal.WriteRatify) {
		t.Error("a person may not ratify their own read")
	}

	for _, p := range []principal.Principal{
		shed,
		team,
		{ID: "nobody"},
		{Kind: principal.KindHuman},
		{ID: "Kyle Penfound", Kind: principal.KindHuman},
	} {
		if _, err := principal.HumanRead(p, grant); !errors.Is(err, principal.ErrCannotAct) {
			t.Errorf("HumanRead(%+v) error = %v, want ErrCannotAct", p, err)
		}
	}
}

func TestAgentRead(t *testing.T) {
	tests := []struct {
		name       string
		agent      principal.Principal
		agentGrant principal.Grant
		humanGrant principal.Grant
		wantScopes principal.Scopes
		wantRead   principal.Read
		wantWrite  principal.Write
	}{{
		name:       "the intersection of the two grants",
		agent:      shed,
		agentGrant: principal.Grant{Scopes: principal.SomeScopes("api", "infra"), Rights: principal.Rights{Read: principal.ReadAll, Write: principal.WriteAssert}},
		humanGrant: principal.Grant{Scopes: principal.SomeScopes("api"), Rights: principal.Rights{Read: principal.ReadAll, Write: principal.WriteAssert}},
		wantScopes: principal.SomeScopes("api"),
		wantRead:   principal.ReadScopedCode,
		wantWrite:  principal.WriteAssert,
	}, {
		name:       "the class caps the grant",
		agent:      principal.Principal{ID: "eyes", Kind: principal.KindAgent, Class: principal.ClassObserver},
		agentGrant: principal.Grant{Scopes: principal.AllScopes(), Rights: principal.Rights{Read: principal.ReadAll, Write: principal.WriteAssert | principal.WriteSubscribe}},
		humanGrant: principal.Grant{Scopes: principal.AllScopes(), Rights: principal.Rights{Read: principal.ReadAll, Write: principal.WriteAssert | principal.WriteSubscribe}},
		wantScopes: principal.AllScopes(),
		wantRead:   principal.ReadScoped,
		wantWrite:  0,
	}, {
		name:       "the person caps the agent",
		agent:      principal.Principal{ID: "orch", Kind: principal.KindAgent, Class: principal.ClassOrchestrator},
		agentGrant: principal.Grant{Scopes: principal.AllScopes(), Rights: principal.Rights{Read: principal.ReadAll, Write: principal.WriteAssert | principal.WriteSubscribe}},
		humanGrant: principal.Grant{Scopes: principal.AllScopes(), Rights: principal.Rights{Read: principal.ReadScoped, Write: principal.WriteAssert}},
		wantScopes: principal.AllScopes(),
		wantRead:   principal.ReadScoped,
		wantWrite:  principal.WriteAssert,
	}, {
		name:       "a steward agent may not ratify or merge",
		agent:      principal.Principal{ID: "keeper", Kind: principal.KindAgent, Class: principal.ClassSteward},
		agentGrant: principal.Grant{Scopes: principal.AllScopes(), Rights: principal.ClassSteward.Rights()},
		humanGrant: principal.Grant{Scopes: principal.AllScopes(), Rights: principal.Rights{Read: principal.ReadAll, Write: principal.WriteAssert | principal.WriteSubscribe | principal.WriteRatify | principal.WriteMerge}},
		wantScopes: principal.AllScopes(),
		wantRead:   principal.ReadAll,
		wantWrite:  principal.WriteAssert | principal.WriteSubscribe,
	}, {
		name:       "a grant nobody filled in grants nothing",
		agent:      shed,
		wantScopes: principal.Scopes{},
		wantRead:   principal.ReadNone,
		wantWrite:  0,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := principal.AgentRead(tt.agent, kyle, tt.agentGrant, tt.humanGrant)
			if err != nil {
				t.Fatalf("AgentRead: %v", err)
			}
			if got.Agent != tt.agent.ID || got.Human != "kyle" {
				t.Errorf("Effective = %+v, want %s acting for kyle", got, tt.agent.ID)
			}
			if got.Grant.Scopes.All != tt.wantScopes.All || !slices.Equal(got.Grant.Scopes.IDs, tt.wantScopes.IDs) {
				t.Errorf("scopes = %+v, want %+v", got.Grant.Scopes, tt.wantScopes)
			}
			if got.Grant.Rights.Read != tt.wantRead || got.Grant.Rights.Write != tt.wantWrite {
				t.Errorf("rights = {%s %s}, want {%s %s}",
					got.Grant.Rights.Read, got.Grant.Rights.Write, tt.wantRead, tt.wantWrite)
			}
		})
	}
}

func TestAgentReadRefuses(t *testing.T) {
	grant := principal.Grant{Scopes: principal.AllScopes(), Rights: principal.ClassSteward.Rights()}
	tests := []struct {
		name         string
		agent, human principal.Principal
		want         string
	}{
		{"a person is not an agent", kyle, kyle, `"kyle" is of kind "human"`},
		{"an agent does not delegate to an agent", shed, shed, `"shed" is of kind "agent"`},
		{"a team cannot read", team, kyle, "a team cannot read"},
		{"a team cannot delegate a read", shed, team, "a team cannot delegate a read"},
		{"an agent with no class", principal.Principal{ID: "x", Kind: principal.KindAgent}, kyle, `class ""`},
		{"an agent with a class that does not exist", principal.Principal{ID: "x", Kind: principal.KindAgent, Class: "admin"}, kyle, `class "admin"`},
		{"an agent with no id", principal.Principal{Kind: principal.KindAgent, Class: principal.ClassWorker}, kyle, `"" is not a principal id`},
		{"a person with no id", shed, principal.Principal{Kind: principal.KindHuman}, `"" is not a principal id`},
		{"a person whose id is not an id", shed, principal.Principal{ID: "Kyle", Kind: principal.KindHuman}, `"Kyle" is not a principal id`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := principal.AgentRead(tt.agent, tt.human, grant, grant)
			if !errors.Is(err, principal.ErrCannotAct) {
				t.Fatalf("error = %v, want ErrCannotAct", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %s", err, tt.want)
			}
		})
	}
}
