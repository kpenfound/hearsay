//go:build integration

package l3_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
)

func TestTraverseDeterministicOrder(t *testing.T) {
	pool := dbtest.Pool(t)
	graph := l2.New(pool)
	src := dbtest.UniqueID()
	root := "code:" + src
	id := func(kind, name string) string { return root + ":" + kind + "-" + name }
	order := []string{"z", "b", "a"}
	parents, owners, children := []string{}, []string{}, []string{}
	for _, name := range order {
		parents = append(parents, id("parent", name))
		owners = append(owners, src+"-owner-"+name)
		children = append(children, id("child", name))
	}
	put := func(e l2.Entity) {
		t.Helper()
		e.Type, e.Origin = l2.TypeModule, l2.OriginConfig
		if err := graph.PutEntity(t.Context(), e); err != nil {
			t.Fatalf("PutEntity(%s) = %v", e.ID, err)
		}
	}
	for _, p := range parents {
		put(l2.Entity{ID: p, Name: p})
	}
	put(l2.Entity{ID: root, Name: root, PartOf: parents, Owners: owners})
	for _, c := range children {
		put(l2.Entity{ID: c, Name: c, PartOf: []string{root}})
	}

	for _, tt := range []struct {
		edge, typ string
		want      []string
	}{
		{"has_part", "entity", slices.Sorted(slices.Values(children))},
		{"part_of", "entity", slices.Sorted(slices.Values(parents))},
		{"owners", "principal", slices.Sorted(slices.Values(owners))},
	} {
		t.Run(tt.edge, func(t *testing.T) {
			var previous string
			for range 3 {
				out, err := l3.New(pool).Traverse(t.Context(), sam, l3.Query{Start: []l3.NodeRef{{Type: "entity", ID: root}}, Steps: []l3.Step{{Edge: tt.edge}}})
				if err != nil {
					t.Fatalf("Traverse() = %v", err)
				}
				got := []string{}
				for _, n := range out.Nodes[1:] {
					if n.Type != tt.typ {
						t.Fatalf("node = %+v, want a %s", n, tt.typ)
					}
					got = append(got, n.ID)
				}
				if !slices.Equal(got, tt.want) {
					t.Fatalf("Traverse(%s) = %v, want %v", tt.edge, got, tt.want)
				}
				current := fmt.Sprint(out)
				if previous != "" && current != previous {
					t.Fatalf("a repeated traversal changed:\n%s\nthen\n%s", previous, current)
				}
				previous = current
			}
		})
	}
}
