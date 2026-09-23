package main

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

// The assertion worker gets a repository reader for every GitHub source a
// `code/` entry's `repo:` names, built with that source's token, and for no
// other source.
func TestRepoReaders(t *testing.T) {
	sources := map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n" +
			"secrets: {token: SEED_GITHUB_TOKEN, webhook_secret: SEED_GITHUB_WEBHOOK_SECRET}\n",
		"sources/github-oss.yaml": "id: github-oss\ntype: github\ncontainers: [acme/oss]\n" +
			"secrets: {token: SEED_OSS_TOKEN}\n",
		"sources/vault.yaml": "id: vault\ntype: obsidian\ncontainers: [notes]\n",
		"scopes/api.yaml":    "id: api\nsources: [github]\n",
	}
	with := func(code string) map[string]string {
		files := map[string]string{"code/c.yaml": code}
		for k, v := range sources {
			files[k] = v
		}
		return files
	}
	github := "- id: code:acme/api\n  type: project\n  repo: {source: github, project: acme/api}\n  codeowners: .github/CODEOWNERS\n" +
		"- id: code:acme/api:engine\n  type: module\n  path_patterns: [engine/**]\n  repo: {source: github, project: acme/api}\n"
	vault := "- id: code:notes\n  type: project\n  repo: {source: vault, project: notes}\n"
	tokens := map[string]string{"SEED_GITHUB_TOKEN": "t0ken"}

	tests := []struct {
		name    string
		code    string
		env     map[string]string
		want    []string // source ids with a reader; nil is no reader at all
		wantErr string
	}{
		// github-oss is named by nothing in code/, so it gets no reader, and
		// its token not being set in any case does not matter.
		{name: "a GitHub source code/ names", code: github, env: map[string]string{"SEED_GITHUB_TOKEN": "t0ken", "SEED_GITHUB_WEBHOOK_SECRET": "s"}, want: []string{"github"}},
		{name: "the token alone: the webhook secret is the connectors service's", code: github, env: tokens, want: []string{"github"}},
		{name: "no reader for another type of source", code: vault, env: tokens},
		{name: "no reader with nothing in code/ naming a repository", code: "id: code:acme\ntype: project\n", env: tokens},
		{name: "a token that is not in the environment stops startup", code: github, wantErr: `source "github": no value in the environment for [token (SEED_GITHUB_TOKEN)]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := config.Load(writeConfig(t, with(tt.code)))
			if err != nil {
				t.Fatalf("config.Load() = %v", err)
			}
			lookup := func(name string) (string, bool) { v, ok := tt.env[name]; return v, ok }
			reader, err := repoReaders(repo, lookup)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("repoReaders() = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("repoReaders() = %v", err)
			}
			if tt.want == nil {
				if reader != nil {
					t.Errorf("repoReaders() = %#v, want none", reader)
				}
				return
			}
			repos, ok := reader.(assertworker.Repos)
			if !ok {
				t.Fatalf("repoReaders() = %T, want assertworker.Repos", reader)
			}
			var got []string
			for id := range repos {
				got = append(got, id)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("repoReaders() has readers for %q, want %q", got, tt.want)
			}
		})
	}
}
