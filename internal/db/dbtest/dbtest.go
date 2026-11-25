// Package dbtest opens the migrated Postgres database integration tests run
// against.
package dbtest

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/poll"
)

const urlVariable = "HEARSAY_DATABASE_URL"

var (
	created atomic.Int64
	unique  atomic.Int64
)

// URL returns HEARSAY_DATABASE_URL, and skips the test when it is unset.
func URL(t testing.TB) string {
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
	pool, err := db.Connect(t.Context(), URL(t))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// UniqueID is a source id, or an id prefix, that no other call in any test
// process uses, so tests share one database without clearing it.
func UniqueID() string {
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(unique.Add(1), 36)
}

// Create makes an empty database of its own named after prefix on the server
// HEARSAY_DATABASE_URL names and returns its URL. The database is dropped when
// the test ends. It skips the test when the variable is unset.
func Create(t testing.TB, prefix string) string {
	t.Helper()
	adminURL := URL(t)
	ctx := t.Context()
	admin, err := db.Open(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + strconv.FormatInt(created.Add(1), 36)
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
	return url
}

// Scratch is Create with every embedded migration applied, returning a pool on
// the new database.
func Scratch(t testing.TB, prefix string) *pgxpool.Pool {
	t.Helper()
	url := Create(t, prefix)
	ctx := t.Context()
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
	poll.Within(t, "a backend to wait on a lock", 10*time.Second, func() bool {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
  WHERE datname = current_database() AND wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
			t.Error(err)
			return true
		}
		return waiting
	})
}
