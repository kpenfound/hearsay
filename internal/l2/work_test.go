package l2_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l2"
)

func TestSeedIncludesWorkParents(t *testing.T) {
	repo := config.Repo{Code: []config.CodeEntity{{ID: "code:acme/api", Type: config.TypeProject}}}
	project, _ := l2.WorkPlacementOf(connector.Event{Source: "projects", Kind: connector.KindIssue, Payload: connector.Payload{Artifact: "p", Title: "Project", Touches: []connector.Touch{{Source: "github", Project: "acme/api"}, {Source: "github", Project: "acme/missing"}}}})
	stream, _ := l2.WorkPlacementOf(connector.Event{Source: "projects", Kind: connector.KindIssue, Payload: connector.Payload{Artifact: "p#s", Title: "Stream"}})
	entities, err := l2.Seed(t.Context(), repo, nil, []l2.Placement{stream, project})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		id, name string
		parents  []string
	}{
		{"work:projects:p", "Project", []string{"code:acme/api"}},
		{"work:projects:p#s", "Stream", []string{"work:projects:p"}},
	} {
		t.Run(tt.id, func(t *testing.T) {
			i := slices.IndexFunc(entities, func(e l2.Entity) bool { return e.ID == tt.id })
			if i < 0 {
				t.Fatal("missing entity")
			}
			e := entities[i]
			if e.Name != tt.name || e.Origin != l2.OriginSource || len(e.Aliases) != 0 || !slices.Equal(e.PartOf, tt.parents) {
				t.Fatalf("entity = %+v", e)
			}
		})
	}
}
