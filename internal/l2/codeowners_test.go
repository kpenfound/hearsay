package l2_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

const codeowners = `# Ownership for acme/api.
*                  @acme/api-team
/engine/           @kpenfound
engine/server/     @samr  @nobody-mapped
*.md               @docs-person
docs/              ops@example.com
/internal/queue/   @kpenfound @acme/api-team # two owners
/vendor/
`

func TestParseCodeOwners(t *testing.T) {
	got := l2.ParseCodeOwners([]byte(codeowners))
	want := []l2.OwnerRule{
		{Pattern: "*", Owners: []string{"@acme/api-team"}},
		{Pattern: "/engine/", Owners: []string{"@kpenfound"}},
		{Pattern: "engine/server/", Owners: []string{"@samr", "@nobody-mapped"}},
		{Pattern: "*.md", Owners: []string{"@docs-person"}},
		{Pattern: "docs/", Owners: []string{"ops@example.com"}},
		{Pattern: "/internal/queue/", Owners: []string{"@kpenfound", "@acme/api-team"}},
	}
	if !slices.EqualFunc(got, want, func(a, b l2.OwnerRule) bool {
		return a.Pattern == b.Pattern && slices.Equal(a.Owners, b.Owners)
	}) {
		t.Errorf("ParseCodeOwners() = %+v, want %+v", got, want)
	}
}

func TestOwnersOf(t *testing.T) {
	rules := l2.ParseCodeOwners([]byte(codeowners))
	tests := []struct {
		dir  string
		want []string
	}{
		{"engine", []string{"@kpenfound"}},
		{"engine/client", []string{"@kpenfound"}},
		// Later lines win, and an unanchored directory pattern matches at any
		// depth.
		{"engine/server", []string{"@samr", "@nobody-mapped"}},
		{"engine/server/storage", []string{"@samr", "@nobody-mapped"}},
		// A rule for files does not own a directory.
		{"guides", []string{"@acme/api-team"}},
		{"site/docs", []string{"ops@example.com"}},
		{"internal/queue", []string{"@kpenfound", "@acme/api-team"}},
		// An anchored pattern only matches at the root.
		{"third_party/internal/queue", []string{"@acme/api-team"}},
		// A line with no owners owns nothing, so what came before stands.
		{"vendor", []string{"@acme/api-team"}},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			if got := l2.OwnersOf(rules, tt.dir); !slices.Equal(got, tt.want) {
				t.Errorf("OwnersOf(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
	if got := l2.OwnersOf(nil, "engine"); got != nil {
		t.Errorf("OwnersOf(no rules) = %q, want none", got)
	}
}

func ownersPrincipals() []principal.Principal {
	return []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: "github-acme", NativeID: "u1", Handle: "kpenfound"}}},
		{ID: "sam", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: "github-acme", NativeID: "u2", Handle: "samr"}}},
		{ID: "api-team", Kind: principal.KindTeam, Identities: []principal.Identity{{Source: "github-acme", Handle: "acme/api-team"}}},
	}
}

func TestResolveOwners(t *testing.T) {
	resolver, err := principal.NewResolver(ownersPrincipals())
	if err != nil {
		t.Fatalf("NewResolver() = %v", err)
	}
	tests := []struct {
		name      string
		source    string
		spellings []string
		want      []string
	}{
		{"a login", "github-acme", []string{"@KPenfound"}, []string{"kyle"}},
		{"a team", "github-acme", []string{"@acme/api-team"}, []string{"api-team"}},
		{"in the order given, once each", "github-acme", []string{"@samr", "@kpenfound", "@samr"}, []string{"sam", "kyle"}},
		{"nobody is invented for a login nobody mapped", "github-acme", []string{"@nobody-mapped"}, nil},
		{"or for an email address", "github-acme", []string{"ops@example.com"}, nil},
		{"a handle in another source is somebody else", "github-other", []string{"@kpenfound"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l2.ResolveOwners(resolver, tt.source, tt.spellings); !slices.Equal(got, tt.want) {
				t.Errorf("ResolveOwners(%q) = %q, want %q", tt.spellings, got, tt.want)
			}
		})
	}
	if got := l2.ResolveOwners(nil, "github-acme", []string{"@kpenfound"}); got != nil {
		t.Errorf("ResolveOwners(no resolver) = %q, want none", got)
	}
}
