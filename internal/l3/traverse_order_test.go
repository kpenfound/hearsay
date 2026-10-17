package l3_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
	"github.com/kpenfound/hearsay/internal/principal"
)

// The store deliberately returns children and parent/owner arrays out of
// order. Exercise the exported view, including ties from multiple frontiers.
func TestTraverseDeterministicOrder(t *testing.T) {
	for _, tt := range []struct{ edge, typ string }{{"has_part", "entity"}, {"part_of", "entity"}, {"owners", "principal"}} {
		t.Run(tt.edge, func(t *testing.T) {
			db := orderStore{}
			reader := l1.Reader{Effective: principal.Effective{Human: "reader", Grant: principal.Grant{Scopes: principal.AllScopes()}}}
			var previous string
			for range 3 {
				out, err := l3.New(db).Traverse(t.Context(), reader, l3.Query{Start: []l3.NodeRef{{Type: "entity", ID: "root"}}, Steps: []l3.Step{{Edge: tt.edge}}})
				if err != nil {
					t.Fatal(err)
				}
				ids := []string{}
				for _, n := range out.Nodes[1:] {
					if n.Type != tt.typ {
						t.Fatalf("node=%+v", n)
					}
					ids = append(ids, n.ID)
				}
				if !slices.Equal(ids, []string{"a", "b", "z"}) {
					t.Fatalf("order=%v", ids)
				}
				current := fmt.Sprint(out)
				if previous != "" && current != previous {
					t.Fatal("repeated traversal changed")
				}
				previous = current
			}
		})
	}
}

type orderStore struct{}

func (orderStore) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, fmt.Errorf("unexpected write")
}
func (orderStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	rows := &orderRows{}
	if strings.Contains(sql, "SELECT id FROM l2_entities") {
		rows.ids = []string{"z", "b", "a"}
	}
	return rows, nil
}
func (orderStore) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	return orderRow{id: args[0].(string)}
}

type orderRow struct{ id string }

func (r orderRow) Scan(dest ...any) error {
	*(dest[0].(*string)) = r.id
	*(dest[1].(*string)) = string(l2.TypeModule)
	*(dest[2].(*string)) = r.id
	if r.id == "root" {
		*(dest[5].(*[]string)) = []string{"z", "b", "a"}
		*(dest[6].(*[]string)) = []string{"z", "b", "a"}
	}
	*(dest[7].(*string)) = string(l2.OriginConfig)
	return nil
}

type orderRows struct {
	pgx.Rows
	ids    []string
	offset int
}

func (r *orderRows) Next() bool             { r.offset++; return r.offset <= len(r.ids) }
func (r *orderRows) Scan(dest ...any) error { *(dest[0].(*string)) = r.ids[r.offset-1]; return nil }
func (*orderRows) Close()                   {}
func (*orderRows) Err() error               { return nil }
