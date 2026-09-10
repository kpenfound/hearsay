package l1_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

// The grants a reader holds are the ones the identity mapping gives them: their
// own identities as `identity` entries, their teams' as `group` entries, and
// nothing else — an agent reading on their behalf adds nothing and takes
// nothing away here.
func TestReaderForDerivesTheGrantsAPrincipalHolds(t *testing.T) {
	res := testPrincipals(t)
	agent, _ := res.Principal("shed")
	kyle, _ := res.Principal("kyle")

	entry := func(kind connector.ACLKind, native string) connector.ACLEntry {
		return connector.ACLEntry{Kind: kind, Source: source, NativeID: native}
	}
	// kyle is `u1`/`kpenfound` and is in `api-team`, which is `t1`/`acme/api-team`.
	kylesGrants := []connector.ACLEntry{
		entry(connector.ACLGroup, "acme/api-team"),
		entry(connector.ACLGroup, "t1"),
		entry(connector.ACLIdentity, "kpenfound"),
		entry(connector.ACLIdentity, "u1"),
	}

	for _, tc := range []struct {
		name string
		of   func() (principal.Effective, error)
		want []connector.ACLEntry
	}{{
		name: "a person reading directly",
		of:   func() (principal.Effective, error) { return principal.HumanRead(kyle, principal.Grant{}) },
		want: kylesGrants,
	}, {
		name: "an agent reading on their behalf holds what they hold",
		of: func() (principal.Effective, error) {
			return principal.AgentRead(agent, kyle, principal.Grant{}, principal.Grant{})
		},
		want: kylesGrants,
	}, {
		name: "another member of the same team",
		of: func() (principal.Effective, error) {
			sam, _ := res.Principal("sam")
			return principal.HumanRead(sam, principal.Grant{})
		},
		want: []connector.ACLEntry{
			entry(connector.ACLGroup, "acme/api-team"),
			entry(connector.ACLGroup, "t1"),
			entry(connector.ACLIdentity, "samr"),
			entry(connector.ACLIdentity, "u2"),
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			eff, err := tc.of()
			if err != nil {
				t.Fatalf("building the effective principal: %v", err)
			}
			reader, err := l1.ReaderFor(res, eff)
			if err != nil {
				t.Fatalf("ReaderFor() = %v, want no error", err)
			}
			if !slices.Equal(reader.Audience, tc.want) {
				t.Errorf("audience is %v, want %v", reader.Audience, tc.want)
			}
			if reader.Effective.Human != eff.Human || reader.Effective.Agent != eff.Agent {
				t.Errorf("effective principal is %+v, want %+v", reader.Effective, eff)
			}
		})
	}
}

// Reads fail closed: a principal the mapping does not hold, or one that is not
// a person, is refused rather than given an empty audience, which would be a
// reader that silently sees only public documents.
func TestReaderForRefusesAPrincipalThatCannotRead(t *testing.T) {
	res := testPrincipals(t)
	for _, tc := range []struct {
		name string
		res  *principal.Resolver
		eff  principal.Effective
		says string // what the error has to name, so it says which of these it was
	}{
		{name: "no identity mapping", res: nil, eff: principal.Effective{Human: "kyle"}, says: "no identity mapping"},
		{name: "nobody", res: res, eff: principal.Effective{}, says: "not a configured principal"},
		{name: "a principal that is not configured", res: res, eff: principal.Effective{Human: "nobody"}, says: "not a configured principal"},
		{name: "a team", res: res, eff: principal.Effective{Human: "api-team"}, says: `is of kind "team"`},
		{name: "an agent as the human", res: res, eff: principal.Effective{Human: "shed"}, says: `is of kind "agent"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, err := l1.ReaderFor(tc.res, tc.eff)
			if !errors.Is(err, principal.ErrCannotAct) {
				t.Fatalf("ReaderFor() = %v, want principal.ErrCannotAct", err)
			}
			if len(reader.Audience) != 0 {
				t.Errorf("a refused reader holds %d grants, want none", len(reader.Audience))
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("ReaderFor() = %v, want it to say %q", err, tc.says)
			}
		})
	}
}

// A group the mapping spells one way and a source spells another still matches,
// because both spellings are taken. What is not taken is a spelling nobody
// configured.
func TestReaderForTakesEverySpellingOfAConfiguredIdentity(t *testing.T) {
	res, err := principal.NewResolver([]principal.Principal{{
		ID:   "kyle",
		Kind: principal.KindHuman,
		Identities: []principal.Identity{
			{Source: source, NativeID: "u1", Handle: "KPenfound"},
			{Source: "discord-acme", Handle: "kyle"},
			// An identity in no source is not a grant anywhere: the mapping
			// never indexes it, and a grant with no source to scope it to
			// would match an entry from any source at all.
			{Source: "", NativeID: "u9", Handle: "ghost"},
		},
	}})
	if err != nil {
		t.Fatalf("NewResolver() = %v", err)
	}
	reader, err := l1.ReaderFor(res, principal.Effective{Human: "kyle"})
	if err != nil {
		t.Fatalf("ReaderFor() = %v, want no error", err)
	}
	want := []connector.ACLEntry{
		{Kind: connector.ACLIdentity, Source: "discord-acme", NativeID: "kyle"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "KPenfound"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "kpenfound"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "u1"},
	}
	if !slices.Equal(reader.Audience, want) {
		t.Errorf("audience is %v, want %v", reader.Audience, want)
	}
}
