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
// because every spelling of a configured identity is taken — as long as nobody
// else answers to it. What is not taken is a spelling nobody configured.
func TestReaderForTakesEverySpellingNobodyElseAnswersTo(t *testing.T) {
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

// A handle and a native id are different namespaces and may collide
// (internal/principal), but an access list entry carries one string with no
// namespace on it. A spelling two principals answer to is therefore a grant to
// neither: taking it would hand one person's documents to another, which is the
// one direction this filter may not fail in
// (docs/design.md#access-control).
func TestReaderForRefusesASpellingTwoPrincipalsAnswerTo(t *testing.T) {
	res, err := principal.NewResolver([]principal.Principal{{
		ID:   "alice",
		Kind: principal.KindHuman,
		Identities: []principal.Identity{
			{Source: source, NativeID: "u1", Handle: "alice"},
			// The source's id for her happens to be the slug of a team.
			{Source: source, NativeID: "acme/owners"},
		},
	}, {
		// The mapping accepts this: nothing else claims the native id `u2`,
		// and nothing else claims the handle `U1` — alice's `u1` is a native
		// id, which is a different namespace.
		ID:         "mallory",
		Kind:       principal.KindHuman,
		Identities: []principal.Identity{{Source: source, NativeID: "u2", Handle: "U1"}},
	}, {
		ID:         "api-team",
		Kind:       principal.KindTeam,
		Members:    []string{"mallory"},
		Identities: []principal.Identity{{Source: source, NativeID: "t1", Handle: "acme/owners"}},
	}})
	if err != nil {
		t.Fatalf("NewResolver() = %v", err)
	}

	mallory, err := l1.ReaderFor(res, principal.Effective{Human: "mallory"})
	if err != nil {
		t.Fatalf("ReaderFor(mallory) = %v, want no error", err)
	}
	// `U1` folds to `u1`, which is alice's native id: neither spelling of the
	// handle is a grant. Nor is the team's own name, which is a native id of
	// alice's — otherwise every member of the team would read her documents.
	// What is left is what nobody else answers to.
	want := []connector.ACLEntry{
		{Kind: connector.ACLGroup, Source: source, NativeID: "t1"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "U1"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "u2"},
	}
	if !slices.Equal(mallory.Audience, want) {
		t.Errorf("mallory holds %v, want %v", mallory.Audience, want)
	}
	for _, held := range mallory.Audience {
		if held.NativeID == "u1" {
			t.Errorf("mallory holds %v, which is alice's grant", held)
		}
	}

	// The positive control: the collision costs alice nothing. Her native id is
	// hers whatever anybody's handle folds to, and her own handle is hers
	// because nothing else answers to it.
	alice, err := l1.ReaderFor(res, principal.Effective{Human: "alice"})
	if err != nil {
		t.Fatalf("ReaderFor(alice) = %v, want no error", err)
	}
	wantAlice := []connector.ACLEntry{
		{Kind: connector.ACLIdentity, Source: source, NativeID: "acme/owners"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "alice"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "u1"},
	}
	if !slices.Equal(alice.Audience, wantAlice) {
		t.Errorf("alice holds %v, want %v", alice.Audience, wantAlice)
	}
}
