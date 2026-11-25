package l1_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

func TestReaderForDerivesTheGrantsAPrincipalHolds(t *testing.T) {
	res := testPrincipals(t)
	agent, _ := res.Principal("shed")
	kyle, _ := res.Principal("kyle")

	entry := func(kind connector.ACLKind, native string) connector.ACLEntry {
		return connector.ACLEntry{Kind: kind, Source: source, NativeID: native}
	}
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
			reader, err := l1.ReaderFor(t.Context(), emptyReaderDB{}, res, eff)
			if err != nil {
				t.Fatalf("ReaderFor() = %v, want no error", err)
			}
			if !connector.ACL(reader.Audience).Equal(tc.want) {
				t.Errorf("audience is %v, want %v", reader.Audience, tc.want)
			}
			if reader.Effective.Human != eff.Human || reader.Effective.Agent != eff.Agent {
				t.Errorf("effective principal is %+v, want %+v", reader.Effective, eff)
			}
		})
	}
}

func TestReaderForRefusesAPrincipalThatCannotRead(t *testing.T) {
	res := testPrincipals(t)
	for _, tc := range []struct {
		name string
		res  *principal.Resolver
		eff  principal.Effective
		says string
	}{
		{name: "no identity mapping", res: nil, eff: principal.Effective{Human: "kyle"}, says: "no identity mapping"},
		{name: "nobody", res: res, eff: principal.Effective{}, says: "not a configured principal"},
		{name: "a principal that is not configured", res: res, eff: principal.Effective{Human: "nobody"}, says: "not a configured principal"},
		{name: "a team", res: res, eff: principal.Effective{Human: "api-team"}, says: `is of kind "team"`},
		{name: "an agent as the human", res: res, eff: principal.Effective{Human: "shed"}, says: `is of kind "agent"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, err := l1.ReaderFor(t.Context(), emptyReaderDB{}, tc.res, tc.eff)
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

func TestReaderForTakesEverySpellingNobodyElseAnswersTo(t *testing.T) {
	res, err := principal.NewResolver([]principal.Principal{{
		ID:   "kyle",
		Kind: principal.KindHuman,
		Identities: []principal.Identity{
			{Source: source, NativeID: "u1", Handle: "KPenfound"},
			{Source: "discord-acme", Handle: "kyle"},
			{Source: "", NativeID: "u9", Handle: "ghost"},
		},
	}})
	if err != nil {
		t.Fatalf("NewResolver() = %v", err)
	}
	reader, err := l1.ReaderFor(t.Context(), emptyReaderDB{}, res, principal.Effective{Human: "kyle"})
	if err != nil {
		t.Fatalf("ReaderFor() = %v, want no error", err)
	}
	want := []connector.ACLEntry{
		{Kind: connector.ACLIdentity, Source: "discord-acme", NativeID: "kyle"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "KPenfound"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "kpenfound"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "u1"},
	}
	if !connector.ACL(reader.Audience).Equal(want) {
		t.Errorf("audience is %v, want %v", reader.Audience, want)
	}
}

func TestReaderForRefusesASpellingTwoPrincipalsAnswerTo(t *testing.T) {
	res, err := principal.NewResolver([]principal.Principal{{
		ID:   "alice",
		Kind: principal.KindHuman,
		Identities: []principal.Identity{
			{Source: source, NativeID: "u1", Handle: "alice"},
			{Source: source, NativeID: "acme/owners"},
		},
	}, {
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

	mallory, err := l1.ReaderFor(t.Context(), emptyReaderDB{}, res, principal.Effective{Human: "mallory"})
	if err != nil {
		t.Fatalf("ReaderFor(mallory) = %v, want no error", err)
	}
	want := []connector.ACLEntry{
		{Kind: connector.ACLGroup, Source: source, NativeID: "t1"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "U1"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "u2"},
	}
	if !connector.ACL(mallory.Audience).Equal(want) {
		t.Errorf("mallory holds %v, want %v", mallory.Audience, want)
	}

	alice, err := l1.ReaderFor(t.Context(), emptyReaderDB{}, res, principal.Effective{Human: "alice"})
	if err != nil {
		t.Fatalf("ReaderFor(alice) = %v, want no error", err)
	}
	wantAlice := []connector.ACLEntry{
		{Kind: connector.ACLIdentity, Source: source, NativeID: "acme/owners"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "alice"},
		{Kind: connector.ACLIdentity, Source: source, NativeID: "u1"},
	}
	if !connector.ACL(alice.Audience).Equal(wantAlice) {
		t.Errorf("alice holds %v, want %v", alice.Audience, wantAlice)
	}
}

type emptyReaderDB struct{ db.Querier }

func (emptyReaderDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return emptyReaderRows{}, nil
}

type emptyReaderRows struct{ pgx.Rows }

func (emptyReaderRows) Next() bool { return false }
func (emptyReaderRows) Close()     {}
func (emptyReaderRows) Err() error { return nil }

func TestReaderForAddsEveryWorkIdentity(t *testing.T) {
	res, err := principal.NewResolver([]principal.Principal{
		{ID: "reader", Kind: principal.KindHuman},
		{ID: "other", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: "sessions", NativeID: "other-session"}}},
		{ID: "agent", Kind: principal.KindAgent, Class: principal.ClassWorker},
	}, connector.SourceConfig{ID: "projects", Type: "work"},
		connector.SourceConfig{ID: "tasks", Type: "work"},
		connector.SourceConfig{ID: "sessions", Type: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, human, agent string }{
		{"direct", "reader", ""},
		{"delegated", "reader", "agent"},
		{"another reader with explicit agent session mapping", "other", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := l1.ReaderFor(t.Context(), emptyReaderDB{}, res, principal.Effective{Human: tt.human, Agent: tt.agent})
			if err != nil {
				t.Fatal(err)
			}
			want := connector.ACL{{Kind: connector.ACLIdentity, Source: "projects", NativeID: tt.human}}
			if tt.human == "other" {
				want = append(want, connector.ACLEntry{Kind: connector.ACLIdentity, Source: "sessions", NativeID: "other-session"})
			}
			want = append(want, connector.ACLEntry{Kind: connector.ACLIdentity, Source: "tasks", NativeID: tt.human})
			if !want.Equal(r.Audience) {
				t.Fatalf("audience = %+v, want %+v", r.Audience, want)
			}
		})
	}
}

func TestReaderForRefusesUnprovenDatabaseAccess(t *testing.T) {
	for _, tt := range []struct {
		name string
		q    db.Querier
	}{
		{"no database", nil}, {"query failure", &recorder{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := l1.ReaderFor(t.Context(), tt.q, testPrincipals(t), principal.Effective{Human: "kyle"})
			if err == nil || r.Effective.Human != "" || len(r.Audience) != 0 {
				t.Fatalf("ReaderFor() = %+v, %v, want failure with no usable reader", r, err)
			}
		})
	}
}
