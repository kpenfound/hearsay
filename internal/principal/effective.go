package principal

import (
	"errors"
	"fmt"
	"slices"
)

// ErrCannotAct is returned when a principal is asked to do something its kind
// or its class does not allow: a team reading, a human used as an agent, an
// agent with no valid class.
var ErrCannotAct = errors.New("principal cannot act")

// Grant is what one principal may see and do: the scopes it is granted, and
// the rights it holds within them.
//
// Nothing configures a grant yet. Enforcement lands in v0.2.0 and v0.6.0
// (#6), and this is the model those build on, which is why [AgentRead] and
// [HumanRead] take grants rather than deriving them from a principal: a
// default invented here would be a permission nobody granted.
type Grant struct {
	// Scopes are the scope ids the principal may read from.
	Scopes Scopes
	// Rights are what it may do within them.
	Rights Rights
}

// Intersect returns the grant held by both: the scopes in both, and the rights
// in both.
func (g Grant) Intersect(other Grant) Grant {
	return Grant{
		Scopes: g.Scopes.Intersect(other.Scopes),
		Rights: g.Rights.Intersect(other.Rights),
	}
}

// Scopes is a set of scope ids, or every scope there is.
//
// The zero value is the empty set, which grants nothing: reads fail closed
// (docs/design.md#access-control), so a grant nobody filled in is a grant to
// nothing rather than a grant to everything.
type Scopes struct {
	// All grants every scope, present and future. It is what a person's own
	// grant looks like before per-principal grants are configurable.
	All bool
	// IDs are the scope ids granted when All is false. It is ignored when All
	// is set.
	IDs []string
}

// AllScopes is the grant of every scope.
func AllScopes() Scopes { return Scopes{All: true} }

// SomeScopes is the grant of exactly these scope ids.
func SomeScopes(ids ...string) Scopes { return Scopes{IDs: slices.Clone(ids)} }

// Has reports whether the set grants this scope.
func (s Scopes) Has(id string) bool { return s.All || slices.Contains(s.IDs, id) }

// Intersect returns the scopes in both sets, sorted and deduplicated. `All`
// intersected with a list is that list, because `All` is every scope there is
// and cannot narrow one.
func (s Scopes) Intersect(other Scopes) Scopes {
	switch {
	case s.All && other.All:
		return Scopes{All: true}
	case s.All:
		return normalize(other.IDs)
	case other.All:
		return normalize(s.IDs)
	}
	var ids []string
	for _, id := range s.IDs {
		if slices.Contains(other.IDs, id) {
			ids = append(ids, id)
		}
	}
	return normalize(ids)
}

// Empty reports whether the set grants no scope at all.
func (s Scopes) Empty() bool { return !s.All && len(s.IDs) == 0 }

func normalize(ids []string) Scopes {
	out := slices.Clone(ids)
	slices.Sort(out)
	return Scopes{IDs: slices.Compact(out)}
}

// Effective is the principal a read runs as: who asked, on whose behalf, and
// what that combination may see. Every bundle served records it
// (docs/design.md#access-control).
type Effective struct {
	// Human is the id of the person the read is for. It is always set: a read
	// nobody is accountable for is not a read Hearsay serves.
	Human string
	// Agent is the id of the agent doing the reading, and is empty when the
	// person is reading directly.
	Agent string
	// Grant is what the read may actually see and do.
	Grant Grant
}

// HumanRead is the effective principal for a person reading directly.
func HumanRead(human Principal, grant Grant) (Effective, error) {
	if err := mustBe(human, KindHuman, "read"); err != nil {
		return Effective{}, err
	}
	return Effective{Human: human.ID, Grant: grant}, nil
}

// AgentRead is the effective principal for an agent reading on a person's
// behalf: the intersection of the agent's grant and theirs, capped by what the
// agent's class allows (docs/design.md#access-control).
//
// Ratifying and merging topics are taken away from the agent whatever its
// class, including a steward's. The class exists so the door is there, closed:
// ratification is a human action by default, and the day it is opened it is
// opened by a configured grant rather than by an agent's class alone.
func AgentRead(agent, human Principal, agentGrant, humanGrant Grant) (Effective, error) {
	if err := mustBe(human, KindHuman, "delegate a read"); err != nil {
		return Effective{}, err
	}
	if err := mustBe(agent, KindAgent, "read"); err != nil {
		return Effective{}, err
	}
	if !agent.Class.Valid() {
		return Effective{}, fmt.Errorf("%w: agent %q has class %q, which is not one of %v",
			ErrCannotAct, agent.ID, agent.Class, Classes())
	}
	grant := agentGrant.Intersect(humanGrant)
	grant.Rights = grant.Rights.Intersect(agent.Class.Rights())
	grant.Rights.Write &^= WriteRatify | WriteMerge
	return Effective{Human: human.ID, Agent: agent.ID, Grant: grant}, nil
}

func mustBe(p Principal, want Kind, verb string) error {
	if p.Kind == want {
		return nil
	}
	if p.Kind == KindTeam {
		return fmt.Errorf("%w: %q is a team, and a team cannot %s: one of its members does",
			ErrCannotAct, p.ID, verb)
	}
	return fmt.Errorf("%w: %q %s, and only a principal of kind %q may %s",
		ErrCannotAct, p.ID, describeKind(p.Kind), want, verb)
}

func describeKind(k Kind) string {
	if k == "" {
		return "has no kind"
	}
	return fmt.Sprintf("is of kind %q", k)
}
