package config_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector/work"
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

// The owner-source example is a pair: a work source and the source that records
// documents and decisions for its projects, with one publisher an attributor on
// both and the person it names as a human principal.
func TestDocsWorkOwnerSourceLoads(t *testing.T) {
	root := writeFiles(t, filesUnder(t, docExamples(t), "work-owner"))
	repo, err := config.Load(filepath.Join(root, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("Load(owner source example) = %v", err)
	}
	if len(repo.Sources) != 2 || work.OwnerOf(repo.Sources[1]) != "projects" || work.OwnerOf(repo.Sources[0]) != "" {
		t.Fatalf("sources = %+v, want a work source and the owner source of it", repo.Sources)
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

// The authority-by-source example: a policy for the scope that covers a work
// source and its owner source ratifies documents and decisions the owner source
// posted, and nothing the work source did; the default beside it still
// ratifies a merged pull request from anywhere.
func TestDocsWorkAuthorityRatifiesByTheSourceAlone(t *testing.T) {
	root := writeFiles(t, filesUnder(t, docExamples(t), "work-authority"))
	repo, err := config.Load(filepath.Join(root, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("Load(work authority example) = %v", err)
	}
	policy := repo.Authority.ForScope("work")
	for _, tt := range []struct {
		class  config.ArtifactClass
		source string
		want   bool
	}{
		{config.ArtifactSpec, "projects-owner", true},
		{config.ArtifactDecision, "projects-owner", true},
		{config.ArtifactSpec, "projects", false},
		{config.ArtifactDecision, "projects", false},
		{config.ArtifactIssue, "projects-owner", false},
		{config.ArtifactMergedPR, "github", false},
	} {
		if got := policy.RatifiedByArtifact(tt.class, tt.source); got != tt.want {
			t.Errorf("work scope: %s from %s ratifies = %v, want %v", tt.class, tt.source, got, tt.want)
		}
	}
	// Under the built-in default nothing a work source records ratifies, and a
	// merged pull request still does.
	def := config.DefaultPolicy()
	for _, source := range []string{"projects", "projects-owner"} {
		for _, class := range []config.ArtifactClass{config.ArtifactSpec, config.ArtifactDecision} {
			if def.RatifiedByArtifact(class, source) {
				t.Errorf("the built-in default ratifies %s from %s", class, source)
			}
		}
	}
	if !repo.Authority.ForScope("elsewhere").RatifiedByArtifact(config.ArtifactMergedPR, "github") {
		t.Error("a merged pull request stops ratifying outside the work scope")
	}
}

// decision is a class of its own, directly after spec in the built-in ranking,
// so that a ranking written out in full lists every class.
func TestDecisionClassRanksAfterSpec(t *testing.T) {
	classes := config.ArtifactClasses()
	spec := slices.Index(classes, config.ArtifactSpec)
	if spec < 0 || classes[spec+1] != config.ArtifactDecision || !config.ArtifactDecision.Valid() {
		t.Fatalf("classes = %v, want decision directly after spec", classes)
	}
	p := config.DefaultPolicy()
	if !p.Outranks(config.ArtifactSpec, config.ArtifactDecision) || !p.Outranks(config.ArtifactDecision, config.ArtifactMeeting) {
		t.Error("the default ranking does not put decision between spec and meeting")
	}
}
