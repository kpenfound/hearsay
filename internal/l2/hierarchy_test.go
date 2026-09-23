package l2_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l2"
)

// dropped is a cycle warning MergeHierarchy logged.
type dropped struct {
	Entity, Parent, Source string
}

func droppedIn(t *testing.T, log string) []dropped {
	t.Helper()
	out := []dropped{}
	for line := range strings.Lines(log) {
		var rec struct {
			Msg, Entity, Parent, Source string
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec.Msg == "dropped a part_of edge that would close a cycle in the entity hierarchy" {
			out = append(out, dropped{rec.Entity, rec.Parent, rec.Source})
		}
	}
	return out
}

func TestMergeHierarchy(t *testing.T) {
	cases := []struct {
		name    string
		in      l2.HierarchyInputs
		want    l2.Parents
		dropped []dropped
	}{
		{
			name: "configuration outranks the tracker, which outranks repository structure",
			in: l2.HierarchyInputs{
				Config:        l2.Parents{"a": {"config-parent"}},
				Tracker:       l2.Parents{"a": {"tracker-parent"}, "b": {"tracker-parent"}},
				RepoStructure: l2.Parents{"a": {"repo-parent"}, "b": {"repo-parent"}, "c": {"repo-parent"}},
			},
			want: l2.Parents{"a": {"config-parent"}, "b": {"tracker-parent"}, "c": {"repo-parent"}},
		},
		{
			name: "the highest source replaces the lower ones' parents rather than adding to them",
			in: l2.HierarchyInputs{
				Config:        l2.Parents{"a": {"x", "y"}},
				RepoStructure: l2.Parents{"a": {"y", "z"}},
			},
			want: l2.Parents{"a": {"x", "y"}},
		},
		{
			name: "a source that sets no parent does not replace anything",
			in: l2.HierarchyInputs{
				Config:        l2.Parents{"a": nil, "b": {}},
				RepoStructure: l2.Parents{"a": {"p"}, "b": {"q"}},
			},
			want: l2.Parents{"a": {"p"}, "b": {"q"}},
		},
		{
			name: "a parent listed twice is one edge",
			in:   l2.HierarchyInputs{Config: l2.Parents{"a": {"p", "p"}}},
			want: l2.Parents{"a": {"p"}},
		},
		{
			name: "the lower-ranked edge closing a cycle is dropped",
			in: l2.HierarchyInputs{
				Config:        l2.Parents{"engine": {"engine/server"}},
				RepoStructure: l2.Parents{"engine/server": {"engine"}},
			},
			want:    l2.Parents{"engine": {"engine/server"}},
			dropped: []dropped{{"engine/server", "engine", "repo_structure"}},
		},
		{
			name: "a longer cycle through a tracker edge drops the repository's edge",
			in: l2.HierarchyInputs{
				Config:        l2.Parents{"a": {"b"}},
				Tracker:       l2.Parents{"b": {"c"}},
				RepoStructure: l2.Parents{"c": {"a", "d"}},
			},
			// Only the edge that closes the cycle goes: c keeps d.
			want:    l2.Parents{"a": {"b"}, "b": {"c"}, "c": {"d"}},
			dropped: []dropped{{"c", "a", "repo_structure"}},
		},
		{
			name: "a lower-ranked edge dropped is not replaced by a lower source's",
			in: l2.HierarchyInputs{
				Config:        l2.Parents{"a": {"b"}},
				Tracker:       l2.Parents{"b": {"a"}},
				RepoStructure: l2.Parents{"b": {"project"}},
			},
			want:    l2.Parents{"a": {"b"}},
			dropped: []dropped{{"b", "a", "tracker"}},
		},
		{
			name:    "within one rank, the edge taken later closes the cycle",
			in:      l2.HierarchyInputs{Tracker: l2.Parents{"a": {"b"}, "b": {"a"}}},
			want:    l2.Parents{"a": {"b"}},
			dropped: []dropped{{"b", "a", "tracker"}},
		},
		{
			name:    "an entity is not its own parent",
			in:      l2.HierarchyInputs{RepoStructure: l2.Parents{"a": {"a", "p"}}},
			want:    l2.Parents{"a": {"p"}},
			dropped: []dropped{{"a", "a", "repo_structure"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, log := seedLog(t)
			got := l2.MergeHierarchy(ctx, tc.in)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("MergeHierarchy() = %v, want %v", got, tc.want)
			}
			want := tc.dropped
			if want == nil {
				want = []dropped{}
			}
			if d := droppedIn(t, log.String()); fmt.Sprint(d) != fmt.Sprint(want) {
				t.Errorf("logged dropped edges %v, want %v", d, want)
			}
		})
	}
}

func TestNestByPattern(t *testing.T) {
	api := config.SourceRef{Source: "github-acme", Project: "acme/api"}
	web := config.SourceRef{Source: "github-acme", Project: "acme/web"}
	cases := []struct {
		name     string
		entities []l2.Located
		want     l2.Parents
	}{
		{
			name: "an entity is part of the nearest entity its patterns lie under",
			entities: []l2.Located{
				// Deepest first, so a farther ancestor is met after the nearest.
				{ID: "go-files", Repo: api, PathPatterns: []string{"engine/server/*.go", "engine/server/x/**/*.go"}},
				{ID: "write", Repo: api, PathPatterns: []string{"engine/server/write/**"}},
				{ID: "server", Repo: api, PathPatterns: []string{"engine/server/**"}},
				{ID: "engine", Repo: api, PathPatterns: []string{"engine/**"}},
			},
			want: l2.Parents{"go-files": {"server"}, "server": {"engine"}, "write": {"server"}},
		},
		{
			name: "every pattern must lie under the parent",
			entities: []l2.Located{
				{ID: "engine", Repo: api, PathPatterns: []string{"engine/**"}},
				{ID: "split", Repo: api, PathPatterns: []string{"engine/a/**", "docs/engine/**"}},
				{ID: "both", Repo: api, PathPatterns: []string{"engine/**", "docs/**"}},
				{ID: "inner", Repo: api, PathPatterns: []string{"engine/b/**", "docs/c/**"}},
			},
			want: l2.Parents{"engine": {"both"}, "inner": {"both"}, "split": {"both"}},
		},
		{
			name: "only a directory pattern contains anything",
			entities: []l2.Located{
				{ID: "all-go", Repo: api, PathPatterns: []string{"**/*.go"}},
				{ID: "wild", Repo: api, PathPatterns: []string{"engine/*/**"}},
				{ID: "file", Repo: api, PathPatterns: []string{"engine"}},
				{ID: "server", Repo: api, PathPatterns: []string{"engine/server/**"}},
			},
			want: l2.Parents{},
		},
		{
			name: "entities with the same pattern are not nested in each other",
			entities: []l2.Located{
				{ID: "one", Repo: api, PathPatterns: []string{"engine/**"}},
				{ID: "two", Repo: api, PathPatterns: []string{"./engine/**"}},
				{ID: "server", Repo: api, PathPatterns: []string{"engine/server/**"}},
			},
			// Both are nearest to the server.
			want: l2.Parents{"server": {"one", "two"}},
		},
		{
			name: "nesting stays within one repository",
			entities: []l2.Located{
				{ID: "engine", Repo: api, PathPatterns: []string{"engine/**"}},
				{ID: "web-server", Repo: web, PathPatterns: []string{"engine/server/**"}},
				{ID: "nowhere", PathPatterns: []string{"engine/server/**"}},
			},
			want: l2.Parents{},
		},
		{
			name: "what lies under nothing is part of the repository's project",
			entities: []l2.Located{
				{ID: "code:acme/api", Repo: api},
				{ID: "code:acme/web", Repo: web},
				{ID: "engine", Repo: api, PathPatterns: []string{"engine/**"}},
				{ID: "server", Repo: api, PathPatterns: []string{"engine/server/**"}},
				{ID: "queue", Repo: api, PathPatterns: []string{"internal/queue/**"}},
				{ID: "patternless", Repo: api},
			},
			want: l2.Parents{"engine": {"code:acme/api"}, "queue": {"code:acme/api"}, "server": {"engine"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := l2.NestByPattern(tc.entities); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("NestByPattern() = %v, want %v", got, tc.want)
			}
		})
	}
}
