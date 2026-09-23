package api

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/principal"
)

// flat is a graph with no hierarchy and no code links: every scope covers
// itself alone.
type flat struct{}

func (flat) Descendants(_ context.Context, ids []string) ([]string, error) {
	return slices.Clone(ids), nil
}

func (flat) LinkedCode(context.Context, []string) ([]string, error) { return nil, nil }

func TestReaderForUsesConfiguredGrants(t *testing.T) {
	repo := config.Repo{Principals: []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.SomeScopes("code:shared", "code:human")}},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, Grant: principal.Grant{Scopes: principal.SomeScopes("code:shared", "code:agent")}},
		{ID: "boss", Kind: principal.KindAgent, Class: principal.ClassSteward, Grant: principal.Grant{Scopes: principal.SomeScopes("code:agent")}},
	}}
	resolver, err := repo.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	c := &Calls{resolver: resolver, reach: flat{}}
	for _, tt := range []struct {
		name, agent string
		class       principal.Class
		want        principal.Scopes
	}{
		{"human", "", "", principal.SomeScopes("code:human", "code:shared")},
		{"agent intersection", "shed", principal.ClassWorker, principal.SomeScopes("code:shared")},
		{"a steward reaches what the person does", "boss", principal.ClassSteward, principal.SomeScopes("code:human", "code:shared")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := c.readerFor(t.Context(), Caller{Principal: "kyle", Agent: tt.agent})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reader.Effective.Grant.Scopes, tt.want) {
				t.Errorf("reader scopes = %+v, want %+v", reader.Effective.Grant.Scopes, tt.want)
			}
			if reader.Effective.Class != tt.class {
				t.Errorf("reader class = %q, want %q", reader.Effective.Class, tt.class)
			}
		})
	}
}
