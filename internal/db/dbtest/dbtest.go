// Package dbtest opens the migrated Postgres database integration tests run
// against.
package dbtest

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/db"
)

const urlVariable = "HEARSAY_DATABASE_URL"

func databaseURL(t testing.TB) string {
	t.Helper()
	url := os.Getenv(urlVariable)
	if url == "" {
		t.Skip(urlVariable + " is not set")
	}
	return url
}

// Pool opens a pool on the database named by HEARSAY_DATABASE_URL and closes
// it when the test ends. It skips the test when the variable is unset, and
// fails it when the database's schema is behind the embedded migrations: tests
// never build a schema of their own.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Connect(t.Context(), databaseURL(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Scratch creates a database of its own named after prefix on the server
// HEARSAY_DATABASE_URL names, applies every embedded migration to it, and
// returns a pool on it. The database is dropped when the test ends. It skips
// the test when the variable is unset.
func Scratch(t testing.TB, prefix string) *pgxpool.Pool {
	t.Helper()
	adminURL := databaseURL(t)
	ctx := t.Context()
	admin, err := db.Open(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		dropper, err := db.Open(ctx, adminURL)
		if err != nil {
			t.Error(err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	base, query, hasQuery := strings.Cut(adminURL, "?")
	url := base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		url += "?" + query
	}
	m, err := db.NewMigrator(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// WaitForLockWait returns once a backend of pool's database is waiting on a
// lock, which is how a test knows a statement it started is blocked behind
// another transaction. It fails the test if none ever waits.
func WaitForLockWait(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
  WHERE datname = current_database() AND wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
			t.Error(err)
			return
		}
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("no backend ever waited on a lock")
}
