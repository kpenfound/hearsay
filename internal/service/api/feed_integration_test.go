//go:build integration

package api_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/poll"
)

func awaitFeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	poll.Within(t, "the change feed to catch up", 2*time.Minute, func() bool {
		var behind int
		if err := pool.QueryRow(t.Context(),
			`SELECT count(*) FROM l0_events WHERE xact_id >= pg_snapshot_xmin(pg_current_snapshot())`).Scan(&behind); err != nil {
			t.Fatal(err)
		}
		return behind == 0
	})
}
