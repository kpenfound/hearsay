// Package l1test holds the test support for reading and writing L1 documents.
package l1test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/l1"
)

// Put stores doc and returns its id.
func Put(t testing.TB, pool *pgxpool.Pool, doc l1.Document) string {
	t.Helper()
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatalf("Put(%s) = %v", doc.ID, err)
	}
	return doc.ID
}

// Embedded reports whether the document has an embedding.
func Embedded(t testing.TB, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var has bool
	if err := pool.QueryRow(t.Context(), `SELECT embedding IS NOT NULL FROM l1_docs WHERE id = $1`, id).Scan(&has); err != nil {
		t.Fatalf("reading the embedding of %s: %v", id, err)
	}
	return has
}
