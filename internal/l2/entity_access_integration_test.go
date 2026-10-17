//go:build integration

package l2_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

func TestResolveForHidesWorkEntitiesBeforeAliasMatching(t *testing.T) {
	pool := scratchPool(t)
	graph := l2.New(pool)
	reader := l1.Reader{Effective: principal.Effective{Human: "reader", Grant: principal.Grant{Scopes: principal.AllScopes()}}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: "projects", NativeID: "owner"}}
	for _, e := range []l2.Entity{
		{ID: "work:projects:retry", Type: l2.TypeProject, Name: "Retry policy", Origin: l2.OriginSource},
		{ID: "code:retry", Type: l2.TypeModule, Name: "Retry policy", Origin: l2.OriginReference},
		{ID: "work:projects:configured", Type: l2.TypeProject, Name: "Configured work", Origin: l2.OriginConfig},
		{ID: "runtime:other", Type: l2.TypeProject, Name: "Another source", Origin: l2.OriginSource},
	} {
		if err := graph.PutEntity(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name string
		acl  connector.ACL
		want string
	}{
		{"no current document", nil, "code:retry"},
		{"unreadable current document", private, "code:retry"},
		{"readable document restores ambiguity", connector.ACL{{Kind: connector.ACLPublic}}, ""},
		{"access tightening removes ambiguity", private, "code:retry"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.acl != nil {
				putDoc(t, pool, "projects", "retry", tt.acl, nil)
			}
			matches, err := graph.ResolveFor(t.Context(), reader, "Retry policy")
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(matches) != 0 {
					t.Fatalf("ResolveFor = %v, want ambiguity", matches)
				}
			} else if len(matches) != 1 || matches[0].Entity.ID != tt.want {
				t.Fatalf("ResolveFor = %v, want only %s", matches, tt.want)
			}
			for _, name := range []string{"Configured work", "Another source"} {
				matches, err := graph.ResolveFor(t.Context(), reader, name)
				if err != nil || len(matches) != 1 {
					t.Fatalf("ResolveFor(%q) = %v,%v, want other origins and namespaces unchanged", name, matches, err)
				}
			}
		})
	}
}
