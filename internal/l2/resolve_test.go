package l2_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/l2"
)

func resolveFixture() []l2.Entity {
	return []l2.Entity{
		{ID: "code:acme/api", Type: l2.TypeProject, Name: "acme/api", Origin: l2.OriginRepoStructure},
		{ID: "code:acme/api:engine", Type: l2.TypeModule, Name: "engine", PathPatterns: []string{"engine/**"},
			PartOf: []string{"code:acme/api"}, Origin: l2.OriginRepoStructure},
		{ID: "code:acme/api:engine/server", Type: l2.TypeService, Name: "Engine server",
			Aliases: []string{"engine", "The Engine"}, PathPatterns: []string{"engine/server/**"}, Origin: l2.OriginConfig},
		{ID: "code:acme/api:queue", Type: l2.TypeModule, Name: "queue", Aliases: []string{"the job queue", "jobs"},
			PathPatterns: []string{"internal/queue/**"}, Origin: l2.OriginConfig},
		{ID: "code:acme/api:worker", Type: l2.TypeModule, Name: "worker", Aliases: []string{"jobs"}, Origin: l2.OriginConfig},
		{ID: "tracker:github-acme:acme/api#12", Type: l2.TypeTrackerItem, Name: "acme/api#12",
			Aliases: []string{"acme/api#12"}, Origin: l2.OriginReference},
	}
}

// resolved is a match reduced to what a case compares.
type resolved struct {
	id      string
	aliases []string
	paths   []string
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []resolved
	}{
		{
			name: "an alias, ignoring case",
			text: "Is THE ENGINE down again?",
			want: []resolved{{id: "code:acme/api:engine/server", aliases: []string{"The Engine", "engine"}}},
		},
		{
			name: "a name matches like an alias",
			text: "the engine server panics",
			want: []resolved{{id: "code:acme/api:engine/server", aliases: []string{"Engine server", "The Engine", "engine"}}},
		},
		{
			name: "an alias only at word edges",
			text: "engineering is hiring",
			want: []resolved{},
		},
		{
			name: "an alias two configured entities answer to resolves to neither",
			text: "the jobs are slow",
			want: []resolved{},
		},
		{
			// `engine` is configured on engine/server and seeded as the name of
			// the top-level directory: the configured one is what a person meant.
			name: "a configured alias wins over a seeded name",
			text: "engine",
			want: []resolved{{id: "code:acme/api:engine/server", aliases: []string{"engine"}}},
		},
		{
			name: "a path matches every pattern that covers it",
			text: "see engine/server/write.go.",
			want: []resolved{
				{id: "code:acme/api:engine", paths: []string{"engine/server/write.go"}},
				{id: "code:acme/api:engine/server", paths: []string{"engine/server/write.go"}},
			},
		},
		{
			name: "a path in backquotes",
			text: "the bug is in `internal/queue/claim.go`",
			want: []resolved{{id: "code:acme/api:queue", paths: []string{"internal/queue/claim.go"}}},
		},
		{
			name: "a word with no slash or dot is not a path",
			text: "internal",
			want: []resolved{},
		},
		{
			name: "a link is not a path",
			text: "https://example.com/engine/server",
			want: []resolved{},
		},
		{
			name: "a tracker item by its reference",
			text: "fixed in acme/api#12",
			want: []resolved{{id: "tracker:github-acme:acme/api#12", aliases: []string{"acme/api#12"}}},
		},
		{
			name: "empty text resolves nothing",
			text: "  ",
			want: []resolved{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := []resolved{}
			for _, m := range l2.Resolve(resolveFixture(), tt.text) {
				got = append(got, resolved{id: m.Entity.ID, aliases: m.Aliases, paths: m.Paths})
			}
			if !slices.EqualFunc(got, tt.want, func(a, b resolved) bool {
				return a.id == b.id && slices.Equal(a.aliases, b.aliases) && slices.Equal(a.paths, b.paths)
			}) {
				t.Errorf("Resolve(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
		})
	}
}
