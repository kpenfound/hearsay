package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/principal"
)

func TestPrincipalScopes(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
		wantGrant           principal.Grant
	}{
		{"human default", "id: kyle\nidentities: [{source: github, handle: kyle}]\n", "", principal.Grant{Scopes: principal.AllScopes()}},
		{"human explicit", "id: kyle\nscopes: [code:future/project]\nidentities: [{source: github, handle: kyle}]\n", "", principal.Grant{Scopes: principal.SomeScopes("code:future/project")}},
		{"agent explicit", "id: shed\nkind: agent\nclass: worker\nscopes: [tracker:github:acme/api#1234]\nidentities: [{source: github, handle: shed}]\n", "", principal.Grant{Scopes: principal.SomeScopes("tracker:github:acme/api#1234")}},
		{"agent all", "id: shed\nkind: agent\nclass: worker\nscopes: ['*']\nidentities: [{source: github, handle: shed}]\n", "", principal.Grant{Scopes: principal.AllScopes()}},
		{"agent missing", "id: shed\nkind: agent\nclass: worker\nidentities: [{source: github, handle: shed}]\n", `principal "shed": scopes: is required`, principal.Grant{}},
		{"agent empty", "id: shed\nkind: agent\nclass: worker\nscopes: []\nidentities: [{source: github, handle: shed}]\n", `principal "shed": scopes: is required`, principal.Grant{}},
		{"malformed id", "id: shed\nkind: agent\nclass: worker\nscopes: [api]\nidentities: [{source: github, handle: shed}]\n", `scopes[0]: "api" is not an entity id`, principal.Grant{}},
		{"team scopes", "id: team\nkind: team\nscopes: []\nidentities: [{source: github, handle: team}]\n", `principal "team": scopes: a team cannot have scopes`, principal.Grant{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := config.Load(writeFiles(t, with(map[string]string{"principals/p.yaml": tt.body})))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			p, ok := repo.Principal(strings.Split(tt.body, "\n")[0][4:])
			if !ok || !reflect.DeepEqual(p.Grant, tt.wantGrant) {
				t.Errorf("Principal grant = %+v, %v, want %+v", p.Grant, ok, tt.wantGrant)
			}
		})
	}
}
