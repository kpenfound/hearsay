package api

import (
	"reflect"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/principal"
)

func TestReaderForUsesConfiguredGrants(t *testing.T) {
	repo := config.Repo{Principals: []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Grant: principal.Grant{Scopes: principal.SomeScopes("code:shared", "code:human")}},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, Grant: principal.Grant{Scopes: principal.SomeScopes("code:shared", "code:agent")}},
	}}
	// readerFor only builds identity and reach; it does not access the database.
	resolver, err := repo.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	c := &Calls{resolver: resolver}
	for _, tt := range []struct {
		name, agent string
		want        principal.Scopes
	}{
		{"human", "", principal.SomeScopes("code:shared", "code:human")},
		{"agent intersection", "shed", principal.SomeScopes("code:shared")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := c.readerFor(Caller{Principal: "kyle", Agent: tt.agent})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reader.Effective.Grant.Scopes, tt.want) {
				t.Errorf("reader scopes = %+v, want %+v", reader.Effective.Grant.Scopes, tt.want)
			}
		})
	}
}
