//go:build integration

package distiller_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestWorkTouchesSeededRepositoryProject(t *testing.T) {
	tests := []struct {
		name string
		row  l2.EntityType
		want bool
	}{
		{name: "no project row"},
		{name: "repository structure project", row: l2.TypeProject, want: true},
		{name: "non-project row", row: l2.TypeModule},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Isolate the fixed code id from other integration tests' seeds.
			pool := newScratchPool(t)
			src := newSource(t)
			cfg := config.Default()
			cfg.Repo = config.Repo{
				Sources: []connector.SourceConfig{{ID: src, Type: "work"}, {ID: "github", Type: "github"}},
				Code: []config.CodeEntity{{
					ID: "code:acme/api:engine", Type: config.TypeModule, Name: "Engine",
					Repo: config.SourceRef{Source: "github", Project: repo}, PathPatterns: []string{"engine/**"},
				}},
				LLM: llm.Default(),
			}
			const projectID = "code:acme/api"
			if tt.row != "" {
				if err := l2.New(pool).PutEntity(t.Context(), l2.Entity{
					ID: projectID, Type: tt.row, Name: repo, Origin: l2.OriginRepoStructure,
				}); err != nil {
					t.Fatal(err)
				}
			}
			author := &connector.Identity{Source: src, Kind: connector.IdentityAgent, NativeID: "poster"}
			root := revised(event(src, connector.KindIssue, "launch", at(0), author, "Launch", "Changes in acme/api."), "r1", at(1))
			root.Payload.Container = connector.Container{Kind: "project", NativeID: "launch"}
			root.Payload.Touches = []connector.Touch{{Source: "github", Project: repo}}
			ingest(t, pool, []connector.Event{root})

			// Build the expected model request from the intended vocabulary. The
			// distiller itself must discover the project through the L2 store.
			vocabulary := cfg.Repo
			vocabulary.Code = slices.Clone(cfg.Repo.Code)
			if tt.want {
				vocabulary.Code = append(vocabulary.Code, config.CodeEntity{
					ID: projectID, Type: config.TypeProject, Repo: cfg.Repo.Code[0].Repo,
				})
			}
			expected, err := l1.Build(l1.Input{Root: root, Repo: vocabulary})
			if err != nil {
				t.Fatal(err)
			}
			fixtures := llm.NewFixtures()
			budget, _ := cfg.Repo.LLM.Tier(llm.TierDistill)
			if err := fixtures.Add(llm.CompletionFixture{
				Tier: llm.TierDistill, Request: distiller.RequestFor(expected, budget.MaxTokens),
				Response: llm.Response{JSON: json.RawMessage(`{"summary":"Repository changes are planned.","outcome_kind":"none"}`), StopReason: llm.StopEnd},
			}); err != nil {
				t.Fatal(err)
			}
			registry, err := llm.NewFake(cfg.Repo.LLM, fixtures)
			if err != nil {
				t.Fatal(err)
			}
			d, err := distiller.New(pool, registry, &cfg)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := d.Distill(t.Context(), expected.ID); err != nil || !result.Written {
				t.Fatalf("Distill() = %+v, %v, want a written document", result, err)
			}
			got, err := l1.New(pool).Get(t.Context(), expected.ID)
			if err != nil {
				t.Fatal(err)
			}
			var wantRefs []l1.Reference
			wantScope := []string{"work:" + src + ":launch"}
			if tt.want {
				wantRefs = []l1.Reference{{Type: l1.RefSystem, ID: projectID}}
				wantScope = append([]string{projectID}, wantScope...)
			}
			if !slices.Equal(got.References, wantRefs) {
				t.Errorf("document references = %v, want %v", got.References, wantRefs)
			}
			if !slices.Equal(got.Scope, wantScope) {
				t.Errorf("document scope = %v, want %v", got.Scope, wantScope)
			}
		})
	}
}
