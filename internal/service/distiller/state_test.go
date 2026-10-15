package distiller_test

import (
	"reflect"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestRootStateDoesNotChangePrompt(t *testing.T) {
	for _, kind := range []connector.Kind{connector.KindIssue, connector.KindPullRequest, connector.KindDocument} {
		t.Run(string(kind), func(t *testing.T) {
			root := event("test", kind, "artifact", at(0), who("test", "u1", "kpenfound"), "Title", "# Heading\nWords.")
			build := func() []l1.Document {
				if kind == connector.KindDocument {
					docs, err := l1.BuildWikiSections(root, nil, testRepo("test"))
					if err != nil {
						t.Fatal(err)
					}
					return docs
				}
				doc, err := l1.Build(l1.Input{Root: root, Repo: testRepo("test")})
				if err != nil {
					t.Fatal(err)
				}
				return []l1.Document{doc}
			}
			absent := build()
			label := "In review"
			root.Payload.State = &connector.State{Core: connector.CoreStateStarted, Native: &label}
			present := build()
			for i := range absent {
				if !reflect.DeepEqual(distiller.RequestFor(absent[i], 2048), distiller.RequestFor(present[i], 2048)) {
					t.Fatal("state changed model request")
				}
			}
		})
	}
}
