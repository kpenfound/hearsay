package config_test

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// base is the smallest valid configuration, in the directory form. Tests add to
// it or replace one of its files, so that a case fails for the reason it is
// about and not because a configuration with no scopes is invalid.
var base = map[string]string{
	"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
	"scopes/api.yaml":     "id: api\nsources: [github]\n",
}

// with returns base with these files added or replacing what is there.
func with(files map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, files)
	return out
}

func TestLoadReportsEveryProblem(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		// want is one substring per problem, and there must be exactly as many
		// problems as entries: a rule that reports the same mistake twice is
		// as wrong as one that reports it not at all.
		want []string
	}{
		{
			name:  "a key that is not a field",
			files: with(map[string]string{"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\nrefresh_every: 5m\n"}),
			want:  []string{`sources/github.yaml:4: no such field "refresh_every" in a source`},
		},
		{
			name:  "an empty file",
			files: with(map[string]string{"sources/empty.yaml": "\n"}),
			want:  []string{"sources/empty.yaml: the file is empty"},
		},
		{
			name:  "a file of comments only",
			files: with(map[string]string{"sources/empty.yaml": "# nothing here yet\n"}),
			want:  []string{"sources/empty.yaml: the file is empty"},
		},
		{
			name:  "several documents in one file",
			files: with(map[string]string{"sources/two.yaml": "id: a\ntype: a\ncontainers: [x]\n---\nid: b\ntype: b\ncontainers: [y]\n"}),
			want:  []string{"sources/two.yaml:4: a second YAML document starts here"},
		},
		{
			name:  "a file that is not a mapping or a list",
			files: with(map[string]string{"sources/scalar.yaml": "github\n"}),
			want:  []string{"sources/scalar.yaml:1: want one source or a list of them, found a value"},
		},
		{
			name:  "broken YAML",
			files: with(map[string]string{"sources/broken.yaml": "id: github\n  type: github\n"}),
			want:  []string{"sources/broken.yaml:2:"},
		},
		{
			name:  "a directory that is not a configuration directory",
			files: with(map[string]string{"topics/x.yaml": "id: x\n"}),
			want:  []string{"topics/: not a configuration directory"},
		},
		{
			name:  "a subdirectory inside a configuration directory",
			files: with(map[string]string{"sources/legacy/old.yaml": "id: old\n"}),
			want:  []string{"sources/legacy/: subdirectories are not read"},
		},
		{
			name:  "a YAML file at the top level of a directory configuration",
			files: with(map[string]string{"extra.yaml": "sources: []\n"}),
			want:  []string{"extra.yaml: a YAML file at the top level is not read"},
		},
		{
			name:  "nothing configured at all",
			files: map[string]string{"sources/.keep.yaml": ""},
			want: []string{
				"no sources are configured",
				"no scopes are configured",
			},
		},

		// Sources.
		{
			name:  "a source with no id and no type",
			files: with(map[string]string{"sources/github.yaml": "containers: [acme/api]\n"}),
			want: []string{
				"source 1: id: is required",
				"source 1: type: is required",
				// A source whose id is the thing that is wrong is not the
				// source the scope was naming, and saying so is not noise.
				`scope "api": sources[0].source: no source is configured with id "github"`,
			},
		},
		{
			name:  "a source id that is not a source id",
			files: with(map[string]string{"sources/github.yaml": "id: GitHub\ntype: github\ncontainers: [acme/api]\n"}),
			want: []string{
				`source "GitHub": id: "GitHub" is not a source id`,
				`scope "api": sources[0].source: no source is configured with id "github"`,
			},
		},
		{
			// The referrer names the id as written, so this is the one round
			// trip: nothing of that name is configured, because the name is
			// what is wrong with it.
			name: "a source id that is not a source id, named by the scope that wants it",
			files: with(map[string]string{
				"sources/github.yaml": "id: GitHub\ntype: github\ncontainers: [acme/api]\n",
				"scopes/api.yaml":     "id: api\nsources: [GitHub]\n",
			}),
			want: []string{
				`source "GitHub": id: "GitHub" is not a source id`,
				`scope "api": sources[0].source: no source is configured with id "GitHub"`,
			},
		},
		{
			name: "two sources with one id",
			files: with(map[string]string{
				"sources/other.yaml": "id: github\ntype: discord\ncontainers: [x]\n",
			}),
			want: []string{`source "github": id: a source with id "github" is already configured at sources/github.yaml:1`},
		},
		{
			name:  "a source that ingests nothing",
			files: with(map[string]string{"sources/github.yaml": "id: github\ntype: github\ncontainers: []\n"}),
			want:  []string{"source \"github\": containers: is required"},
		},
		{
			name:  "a wildcard container beside a named one",
			files: with(map[string]string{"sources/github.yaml": "id: github\ntype: github\ncontainers: [\"*\", acme/api]\n"}),
			want:  []string{`source "github": containers[0]: "*" covers everything, so it must be the only entry`},
		},
		{
			name:  "a container listed twice",
			files: with(map[string]string{"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api, acme/api]\n"}),
			want:  []string{`source "github": containers[1]: "acme/api" is listed twice`},
		},
		{
			name:  "a refresh that is not a duration",
			files: with(map[string]string{"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\nrefresh: 5 minutes\n"}),
			want:  []string{`source "github": refresh: "5 minutes" is not a duration`},
		},
		{
			name:  "a negative refresh",
			files: with(map[string]string{"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\nrefresh: -5m\n"}),
			want:  []string{`source "github": refresh: -5m is negative`},
		},
		{
			name:  "a secret that holds a value instead of a variable name",
			files: with(map[string]string{"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\nsecrets: {token: ghp_secret}\n"}),
			want:  []string{`source "github": secrets.token: "ghp_secret" is not the name of an environment variable`},
		},

		// Scopes.
		{
			name:  "a scope with no sources",
			files: with(map[string]string{"scopes/api.yaml": "id: api\n"}),
			want:  []string{`scope "api": sources: is required`},
		},
		{
			name:  "a scope naming a source that is not configured",
			files: with(map[string]string{"scopes/api.yaml": "id: api\nsources: [github, discord]\n"}),
			want:  []string{`scope "api": sources[1].source: no source is configured with id "discord"`},
		},
		{
			name:  "a scope naming a container the source does not ingest",
			files: with(map[string]string{"scopes/api.yaml": "id: api\nsources:\n  - source: github\n    containers: [acme/other]\n"}),
			want:  []string{`scope "api": sources[0].containers[0]: source "github" does not ingest "acme/other"`},
		},
		{
			name:  "a scope naming one source twice",
			files: with(map[string]string{"scopes/api.yaml": "id: api\nsources:\n  - github\n  - source: github\n    containers: [acme/api]\n"}),
			want:  []string{`scope "api": sources[1].source: source "github" is listed twice`},
		},
		{
			name:  "a key that is not a field of a scope source",
			files: with(map[string]string{"scopes/api.yaml": "id: api\nsources:\n  - source: github\n    containars: [acme/api]\n"}),
			want:  []string{`scopes/api.yaml:4: no such field "containars" in a scope source`},
		},
		{
			name:  "a tracker outside the scope",
			files: with(map[string]string{"scopes/api.yaml": "id: api\nsources:\n  - source: github\n    containers: [acme/api]\ntracker: {source: github, project: acme/infra}\n"}),
			want: []string{
				`scope "api": tracker.project: source "github" does not ingest "acme/infra"`,
				`scope "api": tracker: this scope does not cover "acme/infra" in source "github"`,
			},
		},
		{
			// Both are wrong in the same way and both are said in one pass. A
			// policy's scope is checked for shape before it is looked up, so
			// this is two rules agreeing rather than a reference resolving.
			name: "a scope id that is not a scope id, named by the policy that wants it",
			files: with(map[string]string{
				"scopes/api.yaml":  "id: API\nsources: [github]\n",
				"authority/a.yaml": "scope: API\n",
			}),
			want: []string{
				`scope "API": id: "API" is not a scope id`,
				`authority policy "API": scope: "API" is not a scope id`,
			},
		},
		{
			name:  "a scope naming an entity that is not configured",
			files: with(map[string]string{"scopes/api.yaml": "id: api\nsources: [github]\nentities: [code:acme/api]\n"}),
			want:  []string{`scope "api": entities[0]: no code entity is configured with id "code:acme/api"`},
		},

		// Principals.
		{
			name:  "a principal with no identities",
			files: with(map[string]string{"principals/p.yaml": "id: kyle\n"}),
			want:  []string{`principal "kyle": identities: is required`},
		},
		{
			name:  "an identity that is nobody in particular",
			files: with(map[string]string{"principals/p.yaml": "id: kyle\nidentities:\n  - source: github\n"}),
			want:  []string{`principal "kyle": identities[0]: needs a native_id or a handle`},
		},
		{
			name: "two principals claiming one identity",
			files: with(map[string]string{"principals/p.yaml": "" +
				"- id: kyle\n  identities: [{source: github, handle: kpenfound}]\n" +
				"- id: robin\n  identities: [{source: github, handle: kpenfound}]\n"}),
			want: []string{`principal "robin": identities[0].handle: "kpenfound" in source "github" is already principal "kyle"`},
		},
		{
			name: "a principal id that is not a principal id, named as an owner",
			files: with(map[string]string{
				"principals/p.yaml": "id: Kyle\nidentities: [{source: github, handle: kpenfound}]\n",
				"code/c.yaml":       "id: code:acme/api\ntype: project\nowners: [Kyle]\n",
			}),
			want: []string{
				`principal "Kyle": id: "Kyle" is not a principal id`,
				`code entity "code:acme/api": owners[0]: no principal is configured with id "Kyle"`,
			},
		},
		{
			name:  "an agent with no class",
			files: with(map[string]string{"principals/p.yaml": "id: shed\nkind: agent\nidentities: [{source: github, handle: shed}]\n"}),
			want:  []string{`principal "shed": class: is required on an agent`},
		},
		{
			name:  "a human with an agent class",
			files: with(map[string]string{"principals/p.yaml": "id: kyle\nclass: steward\nidentities: [{source: github, handle: kpenfound}]\n"}),
			want:  []string{`principal "kyle": class: is an agent's access class, and this principal is a human`},
		},
		{
			name:  "an agent class that does not exist",
			files: with(map[string]string{"principals/p.yaml": "id: shed\nkind: agent\nclass: superuser\nidentities: [{source: github, handle: shed}]\n"}),
			want:  []string{`principal "shed": class: "superuser" is not an agent class`},
		},
		{
			name:  "a principal kind that does not exist",
			files: with(map[string]string{"principals/p.yaml": "id: kyle\nkind: robot\nidentities: [{source: github, handle: kpenfound}]\n"}),
			want:  []string{`principal "kyle": kind: "robot" is not a principal kind: want one of human, agent, team`},
		},
		{
			// Every other rule about a principal depends on its kind, so a kind
			// nobody knows is reported once and the rest are not run: with them
			// this is four problems, all of them the same mistake.
			name:  "a principal kind that does not exist, and nothing else fits it",
			files: with(map[string]string{"principals/p.yaml": "id: kyle\nkind: robot\nclass: worker\nmembers: [robin]\n"}),
			want:  []string{`principal "kyle": kind: "robot" is not a principal kind`},
		},
		{
			// One login written in two cases is one identity, because that is
			// how the resolver matches it. Catching it here is what stops it
			// from becoming an ambiguous author at ingest.
			name: "two principals claiming one handle in different cases",
			files: with(map[string]string{"principals/p.yaml": "" +
				"- id: kyle\n  identities: [{source: github, handle: KPenfound}]\n" +
				"- id: robin\n  identities: [{source: github, handle: kpenfound}]\n"}),
			want: []string{`principal "robin": identities[0].handle: "kpenfound" in source "github" is already principal "kyle": one identity is one person, and handles are matched ignoring case`},
		},
		{
			name:  "a team that stands for nobody",
			files: with(map[string]string{"principals/p.yaml": "id: api-team\nkind: team\n"}),
			want:  []string{`principal "api-team": members: is required on a team with no identities`},
		},
		{
			name:  "a member who is not a principal",
			files: with(map[string]string{"principals/p.yaml": "id: api-team\nkind: team\nmembers: [kyle]\n"}),
			want:  []string{`principal "api-team": members[0]: no principal is configured with id "kyle"`},
		},
		{
			name: "a team inside a team",
			files: with(map[string]string{"principals/p.yaml": "" +
				"- id: kyle\n  identities: [{source: github, handle: kpenfound}]\n" +
				"- id: api-team\n  kind: team\n  members: [kyle]\n" +
				"- id: eng\n  kind: team\n  members: [api-team]\n"}),
			want: []string{`principal "eng": members[0]: "api-team" is a team, and a team may not contain a team`},
		},
		{
			// Reported once, as being a member of itself, rather than twice by
			// also being a team inside a team.
			name:  "a team inside itself",
			files: with(map[string]string{"principals/p.yaml": "id: eng\nkind: team\nmembers: [eng]\n"}),
			want:  []string{`principal "eng": members[0]: a team cannot be a member of itself`},
		},
		{
			name:  "a person with members",
			files: with(map[string]string{"principals/p.yaml": "id: kyle\nmembers: [robin]\nidentities: [{source: github, handle: kpenfound}]\n"}),
			want:  []string{`principal "kyle": members: is a team's membership, and this principal is a human`},
		},
		{
			name:  "a team with an agent class",
			files: with(map[string]string{"principals/p.yaml": "id: eng\nkind: team\nclass: steward\nmembers: [kyle]\n"}),
			want: []string{
				`principal "eng": class: is an agent's access class, and this principal is a team`,
				`principal "eng": members[0]: no principal is configured with id "kyle"`,
			},
		},

		// Code entities.
		{
			name:  "a code entity id in the wrong namespace",
			files: with(map[string]string{"code/c.yaml": "id: acme/api\ntype: project\n"}),
			want:  []string{`code entity "acme/api": id: "acme/api" is not a code entity id`},
		},
		{
			name:  "a code entity type that does not exist",
			files: with(map[string]string{"code/c.yaml": "id: code:acme/api\ntype: repository\n"}),
			want:  []string{`code entity "code:acme/api": type: "repository" is not a code entity type`},
		},
		{
			name:  "an owner who is not a principal",
			files: with(map[string]string{"code/c.yaml": "id: code:acme/api\ntype: project\nowners: [kyle]\n"}),
			want:  []string{`code entity "code:acme/api": owners[0]: no principal is configured with id "kyle"`},
		},
		{
			name:  "a parent that is not configured",
			files: with(map[string]string{"code/c.yaml": "id: code:acme/api:x\ntype: module\npart_of: [code:acme/api]\n"}),
			want:  []string{`code entity "code:acme/api:x": part_of[0]: no code entity is configured with id "code:acme/api"`},
		},
		{
			name: "a code entity id in the wrong namespace, named as a parent and by a scope",
			files: with(map[string]string{
				"code/c.yaml": "" +
					"- id: acme/api\n  type: project\n" +
					"- id: code:acme/api:x\n  type: module\n  part_of: [acme/api]\n",
				"scopes/api.yaml": "id: api\nsources: [github]\nentities: [acme/api]\n",
			}),
			want: []string{
				`code entity "acme/api": id: "acme/api" is not a code entity id`,
				`code entity "code:acme/api:x": part_of[0]: no code entity is configured with id "acme/api"`,
				`scope "api": entities[0]: no code entity is configured with id "acme/api"`,
			},
		},
		{
			// An entity the loader is holding out of the configuration is still
			// an entity somebody wrote, and its own edges are still checked.
			name: "a parent that is not configured, on an entity whose own id is malformed",
			files: with(map[string]string{
				"code/c.yaml": "id: acme/api\ntype: project\npart_of: [code:acme]\n",
			}),
			want: []string{
				`code entity "acme/api": id: "acme/api" is not a code entity id`,
				`code entity "acme/api": part_of[0]: no code entity is configured with id "code:acme"`,
			},
		},
		{
			name: "a hierarchy that loops",
			files: with(map[string]string{"code/c.yaml": "" +
				"- id: code:a\n  type: project\n  part_of: [code:c]\n" +
				"- id: code:b\n  type: module\n  part_of: [code:a]\n" +
				"- id: code:c\n  type: module\n  part_of: [code:b]\n"}),
			want: []string{"part_of is a cycle: code:a -> code:c -> code:b -> code:a"},
		},
		{
			name:  "an entity that is part of itself",
			files: with(map[string]string{"code/c.yaml": "id: code:a\ntype: project\npart_of: [code:a]\n"}),
			want:  []string{`code entity "code:a": part_of[0]: an entity cannot be part of itself`},
		},
		{
			name: "one alias meaning two things",
			files: with(map[string]string{"code/c.yaml": "" +
				"- id: code:a\n  type: project\n  aliases: [the engine]\n" +
				"- id: code:b\n  type: project\n  aliases: [The Engine]\n"}),
			want: []string{`code entity "code:b": aliases[0]: "The Engine" is already an alias of "code:a"`},
		},
		{
			name:  "path patterns with no repository to resolve against",
			files: with(map[string]string{"code/c.yaml": "id: code:a\ntype: module\npath_patterns: [engine/**]\n"}),
			want:  []string{`code entity "code:a": path_patterns: need a repo to resolve against`},
		},
		{
			name:  "a CODEOWNERS path that climbs out of the repository",
			files: with(map[string]string{"code/c.yaml": "id: code:a\ntype: project\nrepo: {source: github, project: acme/api}\ncodeowners: ../CODEOWNERS\n"}),
			want:  []string{`code entity "code:a": codeowners: "../CODEOWNERS" is not a path inside the repository`},
		},

		// Authority.
		{
			name:  "a policy for a scope that does not exist",
			files: with(map[string]string{"authority/a.yaml": "scope: web\n"}),
			want:  []string{`authority policy "web": scope: no scope is configured with id "web"`},
		},
		{
			name: "two policies for one scope",
			files: with(map[string]string{"authority/a.yaml": "" +
				"- scope: api\n  ranking: [merged_pr]\n" +
				"- scope: api\n  ranking: [meeting]\n"}),
			want: []string{`authority policy "api": scope: an authority policy for "api" is already configured at authority/a.yaml:1`},
		},
		{
			name:  "an empty ranking",
			files: with(map[string]string{"authority/a.yaml": "scope: api\nranking: []\n"}),
			want:  []string{`authority policy "api": ranking: is empty: leave it out to inherit`},
		},
		{
			name:  "an artifact class that does not exist",
			files: with(map[string]string{"authority/a.yaml": "scope: api\nranking: [merged_pr, standup]\n"}),
			want:  []string{`authority policy "api": ranking[1]: "standup" is not an artifact class`},
		},
		{
			name:  "an artifact class ranked twice",
			files: with(map[string]string{"authority/a.yaml": "scope: api\nranking: [merged_pr, merged_pr]\n"}),
			want:  []string{`authority policy "api": ranking[1]: "merged_pr" is listed twice`},
		},
		{
			name:  "a wildcard in a ranking",
			files: with(map[string]string{"authority/a.yaml": "scope: api\nranking: [\"*\"]\n"}),
			want:  []string{`authority policy "api": ranking[0]: "*" is not allowed here`},
		},
		{
			name:  "a class that ratifies on its own but is not ranked",
			files: with(map[string]string{"authority/a.yaml": "scope: api\nranking: [meeting]\nratified_by:\n  artifacts: [merged_pr]\n"}),
			want:  []string{`authority policy "api": ratified_by.artifacts: "merged_pr" ratifies on its own, and the ranking in force for this scope leaves it out`},
		},
		{
			// The ranking and the ratifier are in two different policies and
			// neither is wrong on its own: it is the policy the scope ends up
			// with that contradicts itself, and the file that narrowed the
			// ranking is where it was introduced.
			name: "a narrowed ranking that contradicts an inherited ratifier",
			files: with(map[string]string{"authority/a.yaml": "" +
				"- scope: \"*\"\n  ratified_by:\n    artifacts: [spec]\n" +
				"- scope: api\n  ranking: [merged_pr, meeting]\n"}),
			want: []string{`authority/a.yaml:4: authority policy "api": ratified_by.artifacts: "spec" ratifies on its own`},
		},
		{
			// A policy that names no scope is in force nowhere, so what it
			// would have meant is not a second thing to fix. The ranking here
			// leaves out the merged_pr it would have inherited as a ratifier,
			// and that is not reported.
			name:  "a policy whose scope is not a scope id, and a ranking that would contradict it",
			files: with(map[string]string{"authority/a.yaml": "scope: API\nranking: [meeting]\n"}),
			want:  []string{`authority policy "API": scope: "API" is not a scope id`},
		},
		{
			// The other direction of the same contradiction: the `*` ranking is
			// narrow but consistent with what `*` ratifies, and the scope names
			// a ratifier that the ranking it inherited leaves out. The scope's
			// file is the one that made the two halves meet.
			name: "a ratifier that contradicts an inherited ranking",
			files: with(map[string]string{"authority/a.yaml": "" +
				"- scope: \"*\"\n  ranking: [meeting, merged_pr]\n" +
				"- scope: api\n  ratified_by:\n    artifacts: [spec]\n"}),
			want: []string{`authority/a.yaml:3: authority policy "api": ratified_by.artifacts: "spec" ratifies on its own`},
		},
		{
			// The contradiction is entirely inside the `*` policy. Every scope
			// inherits it, and none of them introduced it, so it is one problem
			// on one line however many scopes there are.
			name: "a contradiction in the default policy that every scope inherits",
			files: with(map[string]string{
				"scopes/api.yaml":   "- id: api\n  sources: [github]\n- id: web\n  sources: [github]\n",
				"principals/p.yaml": "id: kyle\nidentities: [{source: github, handle: kpenfound}]\n",
				"authority/a.yaml": "" +
					"- scope: \"*\"\n  ranking: [meeting]\n" +
					"- scope: api\n  ratified_by:\n    principals: [kyle]\n" +
					"- scope: web\n  ratified_by:\n    principals: [kyle]\n",
			}),
			want: []string{`authority/a.yaml:1: authority policy "*": ratified_by.artifacts: "merged_pr" ratifies on its own`},
		},
		{
			name:  "a ratifier who is not a principal",
			files: with(map[string]string{"authority/a.yaml": "scope: api\nratified_by:\n  principals: [kyle]\n"}),
			want:  []string{`authority policy "api": ratified_by.principals[0]: no principal is configured with id "kyle"`},
		},
		{
			name:  "a ratifying source that is not configured",
			files: with(map[string]string{"authority/a.yaml": "scope: api\nratified_by:\n  sources: [wiki]\n"}),
			want:  []string{`authority policy "api": ratified_by.sources[0]: no source is configured with id "wiki"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeFiles(t, tt.files))
			var invalid *config.InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("Load() = %v, want an *InvalidError", err)
			}
			got := make([]string, len(invalid.Problems))
			for i, p := range invalid.Problems {
				got[i] = p.Error()
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Load() reported %d problems, want %d:\n%s", len(got), len(tt.want), strings.Join(got, "\n"))
			}
			for _, want := range tt.want {
				if !containsSubstring(got, want) {
					t.Errorf("no problem contains %q; got:\n%s", want, strings.Join(got, "\n"))
				}
			}
		})
	}
}

func containsSubstring(problems []string, want string) bool {
	for _, p := range problems {
		if strings.Contains(p, want) {
			return true
		}
	}
	return false
}

// A problem is reachable through the aggregate, so a caller can pick out the
// file and line rather than parsing the message.
// A `principals/` that is valid comes out as the identity model, and the Repo
// builds the resolver ingest resolves identity hints against.
func TestLoadPrincipalsAndResolver(t *testing.T) {
	repo, err := config.Load(writeFiles(t, with(map[string]string{
		"principals/p.yaml": "" +
			"- id: kyle\n  name: Kyle Penfound\n  identities: [{source: github, native_id: MDQ6VXNlcjE=, handle: KPenfound}]\n" +
			// A native id is matched byte for byte, so two that differ only in
			// case are two identities. A GitHub node id is base64: its case is
			// meaning rather than spelling.
			"- id: robin\n  identities: [{source: github, native_id: mdq6vxnlcje=}]\n" +
			"- id: shed\n  kind: agent\n  class: worker\n  identities: [{source: github, handle: \"shed-agent[bot]\"}]\n" +
			"- id: api-team\n  kind: team\n  members: [kyle, shed]\n" +
			// A team that claims a source's group needs no members: the group
			// is the membership, and the source keeps it up to date.
			"- id: eng\n  kind: team\n  identities: [{source: github, native_id: MDQ6VGVhbTE=}]\n",
	})))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	kyle, ok := repo.Principal("kyle")
	if !ok || kyle.Kind != principal.KindHuman || kyle.Name != "Kyle Penfound" {
		t.Errorf("Principal(kyle) = %+v, %v", kyle, ok)
	}
	// A handle is stored as it was written, and folded when it is matched.
	if len(kyle.Identities) != 1 || kyle.Identities[0].Handle != "KPenfound" {
		t.Errorf("kyle's identities = %+v", kyle.Identities)
	}
	if len(kyle.Members) != 0 {
		t.Errorf("kyle has members %v: only a team does", kyle.Members)
	}
	if team, ok := repo.Principal("api-team"); !ok || team.Kind != principal.KindTeam ||
		!slices.Equal(team.Members, []string{"kyle", "shed"}) {
		t.Errorf("Principal(api-team) = %+v, %v", team, ok)
	}

	r, err := repo.Resolver()
	if err != nil {
		t.Fatalf("Resolver: %v", err)
	}
	for _, tt := range []struct {
		hint connector.Identity
		want string
	}{
		{connector.Identity{Source: "github", Handle: "kpenfound"}, "kyle"},
		{connector.Identity{Source: "github", NativeID: "MDQ6VXNlcjE="}, "kyle"},
		{connector.Identity{Source: "github", NativeID: "mdq6vxnlcje="}, "robin"},
		{connector.Identity{Source: "github", Handle: "shed-agent[bot]"}, "shed"},
		{connector.Identity{Source: "github", NativeID: "MDQ6VGVhbTE="}, "eng"},
	} {
		if got := r.Resolve(tt.hint); got.Status != principal.Resolved || got.Principal.ID != tt.want {
			t.Errorf("Resolve(%+v) = %+v, want %s", tt.hint, got, tt.want)
		}
	}
}

func TestInvalidErrorUnwrapsToProblems(t *testing.T) {
	root := writeFiles(t, with(map[string]string{"scopes/api.yaml": "id: api\nsources: [github, discord]\n"}))
	_, err := config.Load(root)

	var problem *config.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("Load() = %v, want an error that unwraps to a *Problem", err)
	}
	if problem.File != "scopes/api.yaml" || problem.Line != 1 {
		t.Errorf("problem is at %s:%d, want scopes/api.yaml:1", problem.File, problem.Line)
	}
	var invalid *config.InvalidError
	if errors.As(err, &invalid) && invalid.Path != root {
		t.Errorf("InvalidError.Path = %q, want %q", invalid.Path, root)
	}
}

func TestLoadRejectsBothFormsAtOnce(t *testing.T) {
	files := with(map[string]string{"hearsay.yaml": "sources: []\n"})
	if _, err := config.Load(writeFiles(t, files)); err == nil ||
		!strings.Contains(err.Error(), "holds both configuration forms") {
		t.Errorf("Load() = %v, want it to refuse a directory holding both forms", err)
	}
}

// The single file has two spellings, and a directory holding both of them is
// two configurations. Reading either one and not the other loses a whole file
// with nothing said, which is the failure the format is shaped to avoid.
func TestLoadRejectsTwoSingleFiles(t *testing.T) {
	const one = "sources:\n  - id: github\n    type: github\n    containers: [acme/api]\nscopes:\n  - id: api\n    sources: [github]\n"
	const two = "sources:\n  - id: discord\n    type: discord\n    containers: [\"1\"]\nscopes:\n  - id: chat\n    sources: [discord]\n"

	root := writeFiles(t, map[string]string{"hearsay.yaml": one, "hearsay.yml": two})
	repo, err := config.Load(root)
	if err == nil {
		t.Fatalf("Load() loaded %d sources and %d scopes, want an error naming both files",
			len(repo.Sources), len(repo.Scopes))
	}
	for _, want := range []string{"hearsay.yaml", "hearsay.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load() = %v, want the error to name %s", err, want)
		}
	}
	// Either one on its own is still the single-file form.
	if _, err := config.Load(writeFiles(t, map[string]string{"hearsay.yml": one})); err != nil {
		t.Errorf("Load(a directory holding hearsay.yml) = %v, want no error", err)
	}
}

func TestLoadMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nowhere")
	_, err := config.Load(missing)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load(%q) = %v, want an error that is os.ErrNotExist", missing, err)
	}
}

// A configuration directory holds files that are not configuration, and reading
// them as configuration would make a README a syntax error.
func TestLoadIgnoresWhatIsNotConfiguration(t *testing.T) {
	files := with(map[string]string{
		"README.md":        "# our configuration\n",
		".gitignore":       "*.tmp\n",
		"sources/notes.md": "not configuration\n",
	})
	if _, err := config.Load(writeFiles(t, files)); err != nil {
		t.Errorf("Load() = %v, want no error", err)
	}
}

// One file holds one object or a list of them, and a file per source and a file
// of sources have to mean the same thing.
func TestLoadAcceptsAFileOfOneObjectOrMany(t *testing.T) {
	one, err := config.Load(writeFiles(t, map[string]string{
		"sources/github.yaml":  "id: github\ntype: github\ncontainers: [acme/api]\n",
		"sources/discord.yaml": "id: discord\ntype: discord\ncontainers: [\"1\"]\n",
		"scopes/api.yaml":      "id: api\nsources: [github, discord]\n",
	}))
	if err != nil {
		t.Fatalf("Load(a file per source) = %v, want no error", err)
	}
	many, err := config.Load(writeFiles(t, map[string]string{
		"sources/all.yaml": "- id: discord\n  type: discord\n  containers: [\"1\"]\n- id: github\n  type: github\n  containers: [acme/api]\n",
		"scopes/api.yaml":  "id: api\nsources: [github, discord]\n",
	}))
	if err != nil {
		t.Fatalf("Load(one file of sources) = %v, want no error", err)
	}
	if len(one.Sources) != 2 || len(many.Sources) != 2 {
		t.Fatalf("loaded %d and %d sources, want 2 each", len(one.Sources), len(many.Sources))
	}
	for _, id := range []string{"github", "discord"} {
		a, okA := one.Source(id)
		b, okB := many.Source(id)
		if !okA || !okB || a.Type != b.Type {
			t.Errorf("source %q differs between the two: %+v and %+v", id, a, b)
		}
	}
}

// Writing a source id on its own is the same as naming every container it
// ingests, which is what makes the short form safe to reach for.
func TestScopeSourceShorthandMatchesTheLongForm(t *testing.T) {
	short, err := config.Load(writeFiles(t, base))
	if err != nil {
		t.Fatalf("Load(short form) = %v, want no error", err)
	}
	long, err := config.Load(writeFiles(t, with(map[string]string{
		"scopes/api.yaml": "id: api\nsources:\n  - source: github\n    containers: [\"*\"]\n",
	})))
	if err != nil {
		t.Fatalf("Load(long form) = %v, want no error", err)
	}
	for _, container := range []string{"acme/api", "acme/anything"} {
		s, _ := short.Scope("api")
		l, _ := long.Scope("api")
		if s.Covers("github", container) != l.Covers("github", container) {
			t.Errorf("the two forms disagree about %q: short %v, long %v",
				container, s.Covers("github", container), l.Covers("github", container))
		}
	}
}

// The digest identifies the bytes, because that is what a running process can
// report and an operator can compare with what is checked in.
func TestDigestIdentifiesTheBytes(t *testing.T) {
	load := func(t *testing.T, files map[string]string) config.Repo {
		t.Helper()
		repo, err := config.Load(writeFiles(t, files))
		if err != nil {
			t.Fatalf("Load() = %v, want no error", err)
		}
		return repo
	}

	same := load(t, base).Digest
	if again := load(t, base).Digest; again != same {
		t.Errorf("the same configuration digested as %s and %s", same, again)
	}
	moved := load(t, map[string]string{
		"sources/gh.yaml": base["sources/github.yaml"],
		"scopes/api.yaml": base["scopes/api.yaml"],
	}).Digest
	if moved == same {
		t.Errorf("moving a source to another file left the digest at %s", same)
	}
	edited := load(t, with(map[string]string{
		"sources/github.yaml": base["sources/github.yaml"] + "refresh: 5m\n",
	})).Digest
	if edited == same {
		t.Errorf("editing a source left the digest at %s", same)
	}
}

// The single-file form is one mapping of the same five lists, and it is read by
// the same rules, including the one about a key that is not a field.
func TestSingleFileForm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hearsay.yaml")
	write := func(t *testing.T, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	write(t, "sources:\n  - id: github\n    type: github\n    containers: [acme/api]\nscopes:\n  - id: api\n    sources: [github]\n")
	repo, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if len(repo.Sources) != 1 || len(repo.Scopes) != 1 {
		t.Errorf("loaded %d sources and %d scopes, want 1 each", len(repo.Sources), len(repo.Scopes))
	}

	// The line reported is the object's, not the file's, so a problem in the
	// fourth of five sources says which one.
	write(t, "sources:\n  - id: github\n    type: github\n    containers: [acme/api]\n  - id: discord\n    type: discord\n    containers: []\nscopes:\n  - id: api\n    sources: [github]\n")
	_, err = config.Load(path)
	var problem *config.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("Load() = %v, want a problem", err)
	}
	if problem.Line != 5 {
		t.Errorf("problem is on line %d, want 5 (where the second source starts): %v", problem.Line, problem)
	}

	write(t, "sources: []\nscopes: []\ntopics: []\n")
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), `no such field "topics"`) {
		t.Errorf("Load() = %v, want it to reject a key that is not one of the five", err)
	}

	write(t, "- id: github\n")
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "want a mapping with sources, scopes") {
		t.Errorf("Load() = %v, want it to say what a single file looks like", err)
	}
}

// Problems come out in file and line order, so that the same broken
// configuration prints the same list every run — a person walks the files top
// to bottom, and CI output diffs cleanly.
func TestProblemsAreOrderedByFileAndLine(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"sources/github.yaml": "" +
			"- id: github\n  type: github\n  containers: [acme/api]\n  refresh: soon\n" +
			"- id: github\n  type: github\n  containers: [\"*\", acme/api]\n",
		"scopes/api.yaml":   "id: api\nsources: [github, discord]\n",
		"principals/p.yaml": "id: kyle\nidentities: [{source: nope, handle: k}]\n",
	})

	var invalid *config.InvalidError
	if _, err := config.Load(root); !errors.As(err, &invalid) {
		t.Fatalf("Load() = %v, want an *InvalidError", err)
	}
	want := []string{
		"principals/p.yaml:1:",
		"scopes/api.yaml:1:",
		"sources/github.yaml:1:",
		"sources/github.yaml:5:",
		"sources/github.yaml:5:",
	}
	if len(invalid.Problems) != len(want) {
		t.Fatalf("got %d problems, want %d:\n%v", len(invalid.Problems), len(want), invalid)
	}
	for i, prefix := range want {
		if got := invalid.Problems[i].Error(); !strings.HasPrefix(got, prefix) {
			t.Errorf("problem %d is %q, want it to start with %q", i, got, prefix)
		}
	}
}
