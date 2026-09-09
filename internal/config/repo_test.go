package config_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
)

// A scope covers a container or it does not; there is no "probably", because
// what a scope covers decides what a bundle is built from.
func TestScopeCovers(t *testing.T) {
	repo, err := config.Load(writeFiles(t, map[string]string{
		"sources/all.yaml": "" +
			"- id: github\n  type: github\n  containers: [acme/api, acme/infra]\n" +
			"- id: discord\n  type: discord\n  containers: [\"*\"]\n",
		"scopes/api.yaml": "" +
			"id: api\nsources:\n" +
			"  - source: github\n    containers: [acme/api]\n" +
			"  - discord\n",
	}))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	scope, ok := repo.Scope("api")
	if !ok {
		t.Fatal("Scope(api) not found")
	}

	tests := []struct {
		source, container string
		want              bool
	}{
		{"github", "acme/api", true},
		{"github", "acme/infra", false}, // ingested, but not in this scope
		{"discord", "824100000000000001", true},
		{"drive", "anything", false},
		// A scope that takes every container of a source still does not cover
		// an artifact with no container, the way the ingest allowlist does not.
		{"discord", "", false},
		{"github", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := scope.Covers(tt.source, tt.container); got != tt.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", tt.source, tt.container, got, tt.want)
		}
	}
}

func TestTrackerItemID(t *testing.T) {
	repo, err := config.Load(writeFiles(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/all.yaml": "" +
			"- id: api\n  sources: [github]\n  tracker: {source: github, project: acme/api}\n" +
			"- id: web\n  sources: [github]\n",
	}))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}

	api, _ := repo.Scope("api")
	if got, ok := api.TrackerItemID("1234"); !ok || got != "tracker:github:acme/api#1234" {
		t.Errorf("TrackerItemID(1234) = %q, %v, want tracker:github:acme/api#1234, true", got, ok)
	}
	if got, ok := api.TrackerItemID(""); ok || got != "" {
		t.Errorf("TrackerItemID() = %q, %v, want the empty item refused", got, ok)
	}

	web, _ := repo.Scope("web")
	if got, ok := web.TrackerItemID("1234"); ok || got != "" {
		t.Errorf("TrackerItemID on a scope with no tracker = %q, %v, want it refused", got, ok)
	}
}

// The lookups are what every consumer of the configuration starts with, and a
// miss has to be a miss rather than a zero value that looks configured.
func TestRepoLookups(t *testing.T) {
	repo, err := config.Load(writeFiles(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":     "id: api\nsources: [github]\n",
		"principals/p.yaml":   "id: kyle\nidentities: [{source: github, handle: kpenfound}]\n",
		"code/c.yaml":         "id: code:acme/api\ntype: project\n",
	}))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}

	if s, ok := repo.Source("github"); !ok || s.Type != "github" {
		t.Errorf("Source(github) = %+v, %v", s, ok)
	}
	if _, ok := repo.Source("gh"); ok {
		t.Error("Source(gh) found something")
	}
	if _, ok := repo.Scope("api"); !ok {
		t.Error("Scope(api) not found")
	}
	if _, ok := repo.Scope(""); ok {
		t.Error("Scope() found something")
	}
	if p, ok := repo.Principal("kyle"); !ok || p.Kind != config.PrincipalHuman || len(p.Identities) != 1 {
		t.Errorf("Principal(kyle) = %+v, %v", p, ok)
	}
	if _, ok := repo.Principal("robin"); ok {
		t.Error("Principal(robin) found something")
	}
	if e, ok := repo.CodeEntity("code:acme/api"); !ok || e.Type != config.TypeProject {
		t.Errorf("CodeEntity(code:acme/api) = %+v, %v", e, ok)
	}
	if _, ok := repo.CodeEntity("code:nope"); ok {
		t.Error("CodeEntity(code:nope) found something")
	}

	if repo.Path == "" || repo.Digest == "" {
		t.Errorf("Repo.Path = %q and Digest = %q, want both set", repo.Path, repo.Digest)
	}
}

// The zero Repo is a process started with no configuration. It has to answer
// rather than panic, because that is the state `hearsay api` runs in until it
// is pointed at one.
func TestZeroRepo(t *testing.T) {
	var r config.Repo
	if _, ok := r.Source("github"); ok {
		t.Error("the zero Repo has a source")
	}
	if _, ok := r.Scope("api"); ok {
		t.Error("the zero Repo has a scope")
	}
	if r.Allowlist().Allows("github", "acme/api") {
		t.Error("the zero Repo allows ingest, and ingest is default deny")
	}
}
