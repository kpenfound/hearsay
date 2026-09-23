package config_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
)

func TestMatchPath(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"a directory pattern matches the directory", "engine/server/**", "engine/server", true},
		{"and a file in it", "engine/server/**", "engine/server/write.go", true},
		{"and a file deeper in it", "engine/server/**", "engine/server/a/b/c.go", true},
		{"and not its sibling", "engine/server/**", "engine/client/write.go", false},
		{"and not a directory whose name it prefixes", "engine/server/**", "engine/serverless/x.go", false},
		{"and not its parent", "engine/server/**", "engine", false},
		{"a star stays within a segment", "engine/*.go", "engine/server/write.go", false},
		{"a star matches within a segment", "engine/*.go", "engine/write.go", true},
		{"a leading double star matches at any depth", "**/*.go", "a/b/write.go", true},
		{"including the root", "**/*.go", "write.go", true},
		{"a question mark is one character", "cmd/?", "cmd/a", true},
		{"and not two", "cmd/?", "cmd/ab", false},
		{"a literal pattern is only itself", "Makefile", "Makefile", true},
		{"a literal pattern is not a prefix", "Makefile", "Makefile.old", false},
		{"a leading slash is the root", "/engine/**", "engine/x", true},
		{"a leading ./ is the root", "engine/**", "./engine/x", true},
		{"a trailing slash on the path is ignored", "engine/**", "engine/", true},
		{"an empty pattern matches nothing", "", "engine", false},
		{"an empty path matches nothing", "**", "", false},
		{"a double star inside a path", "a/**/z", "a/b/c/z", true},
		{"a double star inside a path matches none", "a/**/z", "a/z", true},
		{"a double star inside a path needs the end", "a/**/z", "a/b/c", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := config.MatchPath(tt.pattern, tt.path); got != tt.want {
				t.Errorf("MatchPath(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}
