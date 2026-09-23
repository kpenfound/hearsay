package principal_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/principal"
)

// graph is a hierarchy and the code links of its pull requests, in memory.
type graph struct {
	parents map[string][]string
	links   map[string][]string
	err     error
}

func (g graph) Descendants(_ context.Context, ids []string) ([]string, error) {
	if g.err != nil {
		return nil, g.err
	}
	out := slices.Clone(ids)
	for i := 0; i < len(out); i++ {
		for child, parents := range g.parents {
			if slices.Contains(parents, out[i]) && !slices.Contains(out, child) {
				out = append(out, child)
			}
		}
	}
	return out, nil
}

func (g graph) LinkedCode(ctx context.Context, ids []string) ([]string, error) {
	var code []string
	for _, id := range ids {
		code = append(code, g.links[id]...)
	}
	return g.Descendants(ctx, code)
}

// The repository has two directories; tracker item 12 has a sub-issue 13, and
// a pull request about 12 touched the engine.
var world = graph{
	parents: map[string][]string{
		"code:api:engine":        {"code:api"},
		"code:api:engine/server": {"code:api:engine"},
		"code:api:web":           {"code:api"},
		"tracker:gh:api#13":      {"tracker:gh:api#12"},
	},
	links: map[string][]string{"tracker:gh:api#12": {"code:api:engine"}},
}

func TestReach(t *testing.T) {
	item := principal.SomeScopes("tracker:gh:api#12")
	repo := principal.SomeScopes("code:api")
	tests := []struct {
		name string
		// class is the agent's, and empty for a person reading directly.
		class        principal.Class
		human, scope principal.Scopes
		want         principal.Scopes
	}{{
		name:  "a person reaches everything when granted every scope",
		human: principal.AllScopes(),
		want:  principal.AllScopes(),
	}, {
		name:  "a person reaches their scopes, what is under them and the code they link to",
		human: item,
		want:  principal.SomeScopes("code:api:engine", "code:api:engine/server", "tracker:gh:api#12", "tracker:gh:api#13"),
	}, {
		name:  "an observer reaches its scopes and nothing they link to",
		class: principal.ClassObserver,
		human: principal.AllScopes(), scope: item,
		want: principal.SomeScopes("tracker:gh:api#12", "tracker:gh:api#13"),
	}, {
		name:  "a worker reaches the code its scopes link to as well",
		class: principal.ClassWorker,
		human: principal.AllScopes(), scope: item,
		want: principal.SomeScopes("code:api:engine", "code:api:engine/server", "tracker:gh:api#12", "tracker:gh:api#13"),
	}, {
		name:  "an orchestrator reaches what a worker does",
		class: principal.ClassOrchestrator,
		human: principal.AllScopes(), scope: item,
		want: principal.SomeScopes("code:api:engine", "code:api:engine/server", "tracker:gh:api#12", "tracker:gh:api#13"),
	}, {
		// Granted the repository and one directory of it, the two meet at the
		// directory, which intersecting the ids as written would not find.
		name:  "a person and an agent meet under both their scopes",
		class: principal.ClassObserver,
		human: repo, scope: principal.SomeScopes("code:api:engine", "code:web"),
		want: principal.SomeScopes("code:api:engine", "code:api:engine/server"),
	}, {
		// The worker is granted item 12, but the two meet at its sub-issue, and
		// only what both reach links to code.
		name:  "a worker links from what both reach, not from its own scopes",
		class: principal.ClassWorker,
		human: principal.SomeScopes("tracker:gh:api#13"), scope: item,
		want: principal.SomeScopes("tracker:gh:api#13"),
	}, {
		name:  "a steward reaches whatever the person does, whatever its own scopes",
		class: principal.ClassSteward,
		human: principal.SomeScopes("code:api:web"), scope: principal.AllScopes(),
		want: principal.SomeScopes("code:api:web"),
	}, {
		name:  "a steward acting for a person granted everything reaches everything",
		class: principal.ClassSteward,
		human: principal.AllScopes(), scope: item,
		want: principal.AllScopes(),
	}, {
		name:  "an agent with no scopes reaches nothing",
		class: principal.ClassWorker,
		human: principal.AllScopes(),
		want:  principal.Scopes{},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eff, err := principal.HumanRead(kyle, principal.Grant{Scopes: tt.human})
			if tt.class != "" {
				bot := principal.Principal{ID: "bot", Kind: principal.KindAgent, Class: tt.class}
				eff, err = principal.AgentRead(bot, kyle, principal.Grant{Scopes: tt.scope}, principal.Grant{Scopes: tt.human})
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := principal.Reach(t.Context(), world, eff, tt.human, tt.scope)
			if err != nil {
				t.Fatal(err)
			}
			if gs := got.Grant.Scopes; gs.All != tt.want.All || !slices.Equal(gs.IDs, tt.want.IDs) {
				t.Errorf("Reach = %+v, want %+v", gs, tt.want)
			}
			if got.Human != eff.Human || got.Agent != eff.Agent || got.Class != eff.Class {
				t.Errorf("Reach changed who reads: %+v, was %+v", got, eff)
			}
		})
	}
}

// A class that is not one of the four reaches nothing, and a graph that cannot
// be read is an error rather than a reach.
func TestReachFailsClosed(t *testing.T) {
	eff := principal.Effective{Human: "kyle", Agent: "bot", Class: "admin"}
	got, err := principal.Reach(t.Context(), world, eff, principal.AllScopes(), principal.AllScopes())
	if err != nil || !got.Grant.Scopes.Empty() {
		t.Errorf("Reach for an unknown class = %+v, %v; want nothing", got.Grant.Scopes, err)
	}
	broken := graph{err: errors.New("the database is gone")}
	eff, _ = principal.HumanRead(kyle, principal.Grant{})
	if _, err := principal.Reach(t.Context(), broken, eff, principal.SomeScopes("code:api"), principal.Scopes{}); err == nil {
		t.Error("Reach over a graph that fails = nil error")
	}
}
