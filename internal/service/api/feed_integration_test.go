//go:build integration

package api_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func awaitFeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var behind int
		if err := pool.QueryRow(t.Context(),
			`SELECT count(*) FROM l0_events WHERE xact_id >= pg_snapshot_xmin(pg_current_snapshot())`).Scan(&behind); err != nil {
			t.Fatal(err)
		}
		if behind == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the feed is still %d events behind after 2m", behind)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
