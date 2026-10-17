package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
)

// The source-specific example deliberately has no identities mapping: work
// identity native ids are principal ids, including for a factory that speaks
// through no other source.
func TestDocsWorkSourceLoads(t *testing.T) {
	examples := docExamples(t)
	root := writeFiles(t, filesUnder(t, examples, "work"))
	repo, err := config.Load(filepath.Join(root, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("Load(work source example) = %v", err)
	}
	if len(repo.Sources) != 1 || repo.Sources[0].Type != "work" || len(repo.Principals) != 1 || len(repo.Principals[0].Identities) != 0 {
		t.Fatal("work example lost its implicit principal identity")
	}
}

func TestWorkImplicitPrincipalIdentities(t *testing.T) {
	tests := []struct {
		name, body string
		work       bool
		want       string
	}{
		{name: "agent with work", body: "id: factory\nkind: agent\nclass: observer\ntoken_env: FACTORY_TOKEN\nscopes: ['*']\n", work: true},
		{name: "agent without work", body: "id: factory\nkind: agent\nclass: observer\ntoken_env: FACTORY_TOKEN\nscopes: ['*']\n", want: "identities: is required without a work source"},
		{name: "human with work", body: "id: kyle\nkind: human\n", work: true},
		{name: "human without work", body: "id: kyle\nkind: human\n", want: "identities: is required without a work source"},
		{name: "team with work still needs membership", body: "id: team\nkind: team\n", work: true, want: "members: is required on a team with no identities"},
		{name: "team without work still needs membership", body: "id: team\nkind: team\n", want: "members: is required on a team with no identities"},
		{name: "agent still needs class", body: "id: factory\nkind: agent\ntoken_env: FACTORY_TOKEN\nscopes: ['*']\n", work: true, want: "class: is required"},
		{name: "agent still needs grants", body: "id: factory\nkind: agent\nclass: observer\ntoken_env: FACTORY_TOKEN\n", work: true, want: "scopes: is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := with(map[string]string{"principals/p.yaml": tt.body})
			if tt.work {
				files["sources/projects.yaml"] = "id: projects\ntype: work\ncontainers: ['*']\n"
			}
			_, err := config.Load(writeFiles(t, files))
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("Load = %v, want %s", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
