//go:build integration

package distiller_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// assertJobs is the serial key of every assert job for one document. It reads
// the table in one statement rather than listing the queue state by state,
// because a worker in another package's test may move the job between states
// while it is being looked for.
func assertJobs(t *testing.T, pool *pgxpool.Pool, docID string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT serial_key FROM queue_job WHERE kind = $1 AND target_id = $2 ORDER BY id`, l2.AssertKindName, docID)
	if err != nil {
		t.Fatalf("reading assert jobs: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("reading assert jobs: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading assert jobs: %v", err)
	}
	return keys
}

// A document whose outcome enters the assertion pipeline is written with its
// assert job, under the scope that covers the artifact; a re-distillation that
// changes nothing asks for none.
func TestADocumentThatAssertsIsWrittenWithItsAssertJob(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	d := newDistiller(t, pool, src)
	id := l1.DocID(src, repo+"#12")

	result, err := d.Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(%s) = %v", id, err)
	}
	if !result.Written || !result.Asserting {
		t.Fatalf("Distill(%s) = %+v, want the document written and an assert job asked for", id, result)
	}
	keys := assertJobs(t, pool, id)
	if len(keys) != 1 || keys[0] != "api" {
		t.Fatalf("assert jobs for %s = %q, want one under the covering scope", id, keys)
	}

	again, err := d.Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(%s, again) = %v", id, err)
	}
	if again.Written || again.Asserting {
		t.Errorf("Distill(%s, again) = %+v, want nothing written and no job", id, again)
	}
	if keys := assertJobs(t, pool, id); len(keys) != 1 {
		t.Errorf("assert jobs after re-distilling = %q, want still one", keys)
	}
}
