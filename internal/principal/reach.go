package principal

import (
	"context"
	"fmt"
	"slices"
)

// Graph is what a reach is read from: the entity hierarchy, and the code
// entities the changes about an entity touched. internal/l2 is the one
// implementation; this package names what it needs rather than importing it,
// because everything above L0 imports this one.
type Graph interface {
	// Descendants is every id given and every entity `part_of` one of them,
	// transitively. An id the graph does not hold yet is still returned: a
	// grant may name an entity before it arrives from L0 (docs/config.md).
	Descendants(ctx context.Context, ids []string) ([]string, error)
	// LinkedCode is the code entities the pull requests about one of these
	// entities touch — the edges #142 records — and every entity under them.
	LinkedCode(ctx context.Context, ids []string) ([]string, error)
}

// Union returns the scopes in either set, sorted and deduplicated. `All` with
// anything is `All`.
func (s Scopes) Union(other Scopes) Scopes {
	if s.All || other.All {
		return AllScopes()
	}
	return normalize(append(slices.Clone(s.IDs), other.IDs...))
}

// Reach is how far a read runs (docs/design.md#access-control): the entity ids
// it may read documents about, or every one. It returns the effective principal
// with Grant.Scopes replaced by that reach, which is what every read filters on
// from then on — a document is in reach when it is about an entity in it, and a
// document about no entity is in reach only of a reach that is `All`.
//
// human and agent are the scopes each is configured with; agent is ignored when
// the person reads directly. A granted scope covers its entity and every
// entity `part_of` it, so the two are intersected after each is expanded down
// the hierarchy: a person granted a repository and an agent granted one
// directory of it meet at that directory, which an intersection of the ids as
// written would not find.
//
// What the class adds is read from [Class.Rights]:
//
//   - [ReadScoped], an observer, reads the intersection and nothing more.
//   - [ReadScopedCode], a worker or an orchestrator, reads it and the code
//     entities it links to ([Graph.LinkedCode]).
//   - [ReadAll], a steward, reads everything ingested whatever its own scopes
//     say — which, held to the person as every agent is, is what the person
//     reaches.
//   - Anything else reads nothing.
//
// A person reading directly reaches their scopes and the code they link to:
// for a person the access lists are the permission, and scopes only filter for
// relevance, so they are not capped by a class they do not have.
//
// Whatever the class, the result is never wider than what the person reaches,
// and the access lists still apply to everything in it.
func Reach(ctx context.Context, g Graph, eff Effective, human, agent Scopes) (Effective, error) {
	read := ReadScopedCode
	if eff.Agent != "" {
		read = eff.Class.Rights().Read
	}
	if read == ReadNone {
		eff.Grant.Scopes = Scopes{}
		return eff, nil
	}
	persons, err := expand(ctx, g, human)
	if err != nil {
		return Effective{}, err
	}
	// What the person reaches with the code their scopes link to. It bounds
	// every agent acting for them, and is a steward's whole reach.
	personReach := func() (Scopes, error) {
		code, err := linked(ctx, g, persons)
		if err != nil {
			return Scopes{}, err
		}
		return persons.Union(code), nil
	}
	if eff.Agent == "" || read == ReadAll {
		reach, err := personReach()
		if err != nil {
			return Effective{}, err
		}
		eff.Grant.Scopes = reach
		return eff, nil
	}

	agents, err := expand(ctx, g, agent)
	if err != nil {
		return Effective{}, err
	}
	both := agents.Intersect(persons)
	if read == ReadScoped {
		eff.Grant.Scopes = both
		return eff, nil
	}
	code, err := linked(ctx, g, both)
	if err != nil {
		return Effective{}, err
	}
	bound, err := personReach()
	if err != nil {
		return Effective{}, err
	}
	eff.Grant.Scopes = both.Union(code).Intersect(bound)
	return eff, nil
}

// expand is a grant with everything under each of its scopes.
func expand(ctx context.Context, g Graph, s Scopes) (Scopes, error) {
	if s.All || len(s.IDs) == 0 {
		return s, nil
	}
	ids, err := g.Descendants(ctx, s.IDs)
	if err != nil {
		return Scopes{}, fmt.Errorf("reading what scopes %v cover: %w", s.IDs, err)
	}
	return normalize(ids), nil
}

// linked is the code entities a set of scopes links to.
func linked(ctx context.Context, g Graph, s Scopes) (Scopes, error) {
	if s.All || len(s.IDs) == 0 {
		return s, nil
	}
	ids, err := g.LinkedCode(ctx, s.IDs)
	if err != nil {
		return Scopes{}, fmt.Errorf("reading the code %d scopes link to: %w", len(s.IDs), err)
	}
	return normalize(ids), nil
}
