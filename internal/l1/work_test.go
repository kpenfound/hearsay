package l1_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func touchCode() []config.CodeEntity {
	return append(slices.Clone(pathCode), config.CodeEntity{
		// The distiller also supplies repository projects seeded in L2 rather
		// than configured in code/. These have no prose or path vocabulary.
		ID: "code:acme/api", Type: config.TypeProject,
		Repo: config.SourceRef{Source: source, Project: repo},
	}, config.CodeEntity{
		ID: "code:acme/other", Type: config.TypeProject, Name: "Other repository",
		Repo: config.SourceRef{Source: source, Project: "acme/other"},
	})
}

func TestUnnamedRepositoryProject(t *testing.T) {
	tests := []struct {
		name    string
		touches []connector.Touch
		ownRepo bool
		paths   []string
		want    []string
	}{
		{
			name:    "repository-only touch references an unnamed project",
			touches: []connector.Touch{{Source: source, Project: repo}},
			want:    []string{"code:acme/api"},
		},
		{
			name: "prose naming acme/api does not reference the project",
		},
		{
			name:    "ordinary repository paths reference only matching modules",
			ownRepo: true,
			paths:   []string{"engine/server/x.md"},
			want:    []string{"code:acme/api:engine/server", "code:acme/api:engine"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := workEvent(connector.KindIssue, "launch")
			root.Payload.Text = "Changes in acme/api."
			root.Payload.Touches = tt.touches
			root.Payload.Paths = tt.paths
			wantScope := []string{"work:projects:launch"}
			if tt.ownRepo {
				root.Source = source
				root.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: repo}
				wantScope = nil
			}
			cfg := config.Repo{Code: touchCode(), Sources: []connector.SourceConfig{{ID: "projects", Type: "work"}}}
			doc, err := l1.Build(l1.Input{Root: root, Repo: cfg})
			if err != nil {
				t.Fatal(err)
			}
			var wantRefs []l1.Reference
			for _, id := range tt.want {
				wantRefs = append(wantRefs, l1.Reference{Type: l1.RefSystem, ID: id})
				wantScope = append(wantScope, id)
			}
			slices.Sort(wantScope)
			assertRefs(t, doc.References, wantRefs)
			if !slices.Equal(doc.Scope, wantScope) {
				t.Errorf("Build().Scope = %v, want %v", doc.Scope, wantScope)
			}
		})
	}
}

func TestReferencesFromTouches(t *testing.T) {
	tests := []struct {
		name        string
		touches     []connector.Touch
		paths       []string
		text        string
		source      string
		omitProject bool
		want        []string
	}{{
		name:    "repository without paths",
		touches: []connector.Touch{{Source: source, Project: repo}},
		want:    []string{"code:acme/api"},
	}, {
		name:        "configured module alone does not invent a repository reference",
		touches:     []connector.Touch{{Source: source, Project: repo}},
		omitProject: true,
	}, {
		name:        "configured modules still match paths without a project entry",
		touches:     []connector.Touch{{Source: source, Project: repo, Paths: []string{"engine/server/x.md"}}},
		omitProject: true,
		want:        []string{"code:acme/api:engine/server", "code:acme/api:engine"},
	}, {
		name:    "paths match most specific first across sources and containers",
		touches: []connector.Touch{{Source: source, Project: repo, Paths: []string{"engine/server/x.go"}}},
		want:    []string{"code:acme/api:engine/server", "code:acme/api:engine", "code:acme/api:golang", "code:acme/api"},
	}, {
		name: "multiple repositories and duplicate entries",
		touches: []connector.Touch{
			{Source: source, Project: "acme/other", Paths: []string{"engine/server/x.md"}},
			{Source: source, Project: repo, Paths: []string{"engine/client/x.md", "engine/server/x.md"}},
			{Source: source, Project: repo, Paths: []string{"engine/server/x.md"}},
		},
		want: []string{"code:acme/api:engine/client", "code:acme/api:engine/server", "code:acme/other:engine/server", "code:acme/api:engine", "code:acme/api", "code:acme/other"},
	}, {
		name:    "unknown repository produces no references",
		touches: []connector.Touch{{Source: source, Project: "acme/missing", Paths: []string{"engine/server/x.go"}}},
	}, {
		name:    "same repository name in another source produces no references",
		touches: []connector.Touch{{Source: "other-forge", Project: repo, Paths: []string{"engine/server/x.go"}}},
	}, {
		name:    "repository matching ignores case like ordinary paths",
		touches: []connector.Touch{{Source: source, Project: "ACME/API", Paths: []string{"engine/server/x.md"}}},
		want:    []string{"code:acme/api:engine/server", "code:acme/api:engine", "code:acme/api"},
	}, {
		name:    "unmatched paths still reference the repository",
		touches: []connector.Touch{{Source: source, Project: repo, Paths: []string{"docs/README.md"}}},
		want:    []string{"code:acme/api"},
	}, {
		name:    "generic touches work for a non-work source",
		source:  "tracker",
		touches: []connector.Touch{{Source: source, Project: repo, Paths: []string{"engine/server/x.md"}}},
		want:    []string{"code:acme/api:engine/server", "code:acme/api:engine", "code:acme/api"},
	}, {
		name:    "text mentions deduplicate with path matches",
		text:    "Engine server and queue",
		touches: []connector.Touch{{Source: source, Project: repo, Paths: []string{"engine/server/x.md"}}},
		want:    []string{"code:acme/api:engine/server", "code:acme/api:engine", "code:acme/api", "code:acme/api:queue"},
	}, {
		name:    "own paths cannot borrow an explicit touch repository",
		touches: []connector.Touch{{Source: source, Project: repo}},
		paths:   []string{"engine/server/x.go"},
		want:    []string{"code:acme/api"},
	}, {
		name:  "own paths stay confined to the event repository",
		paths: []string{"engine/server/x.go"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := workEvent(connector.KindIssue, "launch")
			if tt.source != "" {
				ev.Source = tt.source
			}
			if tt.text != "" {
				ev.Payload.Text = tt.text
			}
			ev.Payload.Touches = tt.touches
			ev.Payload.Paths = tt.paths
			want := make([]l1.Reference, 0, len(tt.want))
			for _, id := range tt.want {
				want = append(want, l1.Reference{Type: l1.RefSystem, ID: id})
			}
			code := touchCode()
			if tt.omitProject {
				code = slices.DeleteFunc(code, func(e config.CodeEntity) bool { return e.ID == "code:acme/api" })
			}
			assertRefs(t, l1.References([]connector.Event{ev}, nil, code), want)

			// Input order must not change the references or their specificity.
			slices.Reverse(ev.Payload.Touches)
			slices.Reverse(code)
			assertRefs(t, l1.References([]connector.Event{ev}, nil, code), want)
		})
	}
}

func workEvent(kind connector.Kind, artifact string) connector.Event {
	ev := revised(event(kind, artifact, at(0), nil, "Launch plan", "Choose a retry policy."), "r1", at(1))
	ev.Source = "projects"
	ev.Payload.Author = &connector.Identity{Source: ev.Source, Kind: connector.IdentityAgent, NativeID: "poster"}
	ev.Payload.Container = connector.Container{Kind: "project", NativeID: "launch"}
	return ev
}

func TestBuildWorkScope(t *testing.T) {
	tests := []struct {
		name       string
		artifact   string
		partOf     string
		paths      []string
		update     bool
		sourceType string
		want       []string
	}{{
		name: "project", artifact: "launch", sourceType: "work",
		want: []string{"code:acme/api", "work:projects:launch"},
	}, {
		name: "workstream", artifact: "launch#retry", partOf: "launch", sourceType: "work",
		paths: []string{"engine/server/retry.md"},
		want:  []string{"code:acme/api", "code:acme/api:engine", "code:acme/api:engine/server", "work:projects:launch", "work:projects:launch#retry"},
	}, {
		name: "workstream update in its workstream document", artifact: "launch#retry", partOf: "launch", update: true, sourceType: "work",
		paths: []string{"engine/server/retry.md"},
		want:  []string{"code:acme/api", "code:acme/api:engine", "code:acme/api:engine/server", "work:projects:launch", "work:projects:launch#retry"},
	}, {
		name: "project update in its project document", artifact: "launch", update: true, sourceType: "work",
		want: []string{"code:acme/api", "work:projects:launch"},
	}, {
		name: "non-work source still gets generic touches references", artifact: "launch#retry", sourceType: "tracker",
		paths: []string{"engine/server/retry.md"},
		want:  []string{"code:acme/api", "code:acme/api:engine", "code:acme/api:engine/server"},
	}, {
		name: "unconfigured source gets no work entity", artifact: "launch#retry",
		paths: []string{"engine/server/retry.md"},
		want:  []string{"code:acme/api", "code:acme/api:engine", "code:acme/api:engine/server"},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := workEvent(connector.KindIssue, tt.artifact)
			root.Payload.PartOf = tt.partOf
			root.Payload.Touches = []connector.Touch{{Source: source, Project: repo, Paths: tt.paths}}
			cfg := config.Repo{Code: touchCode()}
			if tt.sourceType != "" {
				cfg.Sources = []connector.SourceConfig{{ID: root.Source, Type: tt.sourceType}}
			}
			var children []connector.Event
			if tt.update {
				update := reply(workEvent(connector.KindMessage, tt.artifact+":update:progress"), tt.artifact)
				update.Payload.Title = ""
				children = []connector.Event{update}
			}
			doc, err := l1.Build(l1.Input{Root: root, Children: children, Repo: cfg})
			if err != nil {
				t.Fatalf("Build() = %v, want nil", err)
			}
			if !slices.Equal(doc.Scope, tt.want) {
				t.Errorf("Build().Scope = %v, want %v", doc.Scope, tt.want)
			}
			if tt.update && !slices.Contains(doc.L0Refs, connector.EventID(children[0].Source, children[0].NativeID)) {
				t.Errorf("Build().L0Refs = %v, want the update event", doc.L0Refs)
			}
		})
	}
}
