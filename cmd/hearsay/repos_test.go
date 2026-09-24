package main

import (
	"slices"
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

// The assertion worker gets a replier for every GitHub source, built with its
// token and not its webhook secret, and a GitHub source whose token is not in
// the environment stops it starting: it would run commands and answer none. A
// read-only source gets none, and the worker does not ask for its token.
func TestCommandRepliers(t *testing.T) {
	files := map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n" +
			"secrets: {token: CMD_GITHUB_TOKEN, webhook_secret: CMD_GITHUB_WEBHOOK_SECRET}\n",
		"sources/github-oss.yaml": "id: github-oss\ntype: github\ncontainers: [acme/oss]\n" +
			"secrets: {token: CMD_OSS_TOKEN}\n",
		"sources/github-ro.yaml": "id: github-ro\ntype: github\ncontainers: [acme/docs]\nread_only: true\n" +
			"secrets: {token: CMD_RO_TOKEN}\n",
		"sources/vault.yaml": "id: vault\ntype: obsidian\ncontainers: [notes]\n",
		"scopes/api.yaml":    "id: api\nsources: [github]\n",
	}
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{name: "every GitHub source, with its token alone", env: map[string]string{"CMD_GITHUB_TOKEN": "a", "CMD_OSS_TOKEN": "b"}, want: "github,github-oss"},
		{name: "a token that is not in the environment stops startup", env: map[string]string{"CMD_GITHUB_TOKEN": "a"}, wantErr: `source "github-oss": no value in the environment for [token (CMD_OSS_TOKEN)]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := config.Load(writeConfig(t, files))
			if err != nil {
				t.Fatalf("config.Load() = %v", err)
			}
			lookup := func(name string) (string, bool) { v, ok := tt.env[name]; return v, ok }
			replies, err := commandRepliers(repo, lookup)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("commandRepliers() = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("commandRepliers() = %v", err)
			}
			var got []string
			for id := range replies {
				got = append(got, id)
			}
			slices.Sort(got)
			if strings.Join(got, ",") != tt.want {
				t.Errorf("commandRepliers() has repliers for %q, want %q", got, tt.want)
			}
		})
	}
}

// The API answers the slash commands of every Discord source that names an
// application and is not read-only. A read-only source's application
// settings are passed over, and its bot token is not asked for: the API
// registers nothing with it and answers no interaction for it.
func TestDiscordApps(t *testing.T) {
	const app = "settings: {guild: \"824100000000000000\", application_id: \"824100000000000009\", " +
		"public_key: \"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c\"}\n"
	files := map[string]string{
		"sources/chat.yaml": "id: chat\ntype: discord\ncontainers: [\"824100000000000001\"]\n" + app +
			"secrets: {token: APP_CHAT_TOKEN}\n",
		"sources/chat-ro.yaml": "id: chat-ro\ntype: discord\ncontainers: [\"824100000000000002\"]\nread_only: true\n" + app +
			"secrets: {token: APP_RO_TOKEN}\n",
		"sources/chat-plain.yaml": "id: chat-plain\ntype: discord\ncontainers: [\"824100000000000003\"]\n" +
			"settings: {guild: \"824100000000000000\"}\nsecrets: {token: APP_PLAIN_TOKEN}\n",
		"scopes/chat.yaml": "id: chat\nsources: [chat]\n",
	}
	repo, err := config.Load(writeConfig(t, files))
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	lookup := func(name string) (string, bool) {
		if name == "APP_CHAT_TOKEN" {
			return "bot", true
		}
		return "", false
	}
	apps, err := discordApps(repo, lookup)
	if err != nil {
		t.Fatalf("discordApps() = %v", err)
	}
	var got []string
	for _, a := range apps {
		got = append(got, a.Source)
	}
	if strings.Join(got, ",") != "chat" {
		t.Errorf("discordApps() has applications for %q, want only chat", got)
	}
}
