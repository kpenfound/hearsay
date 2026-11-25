package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is what a layer's store runs its statements on: a pgx pool, or a
// transaction taken from one, so that a write and the work enqueued for it can
// be one transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// ErrNoDatabaseURL is returned when a command that needs Postgres was given no
// connection URL. It is a sentinel so that a caller can tell "you have not
// configured a database" from "the database refused me".
var ErrNoDatabaseURL = errors.New("no database url: pass --database-url or set HEARSAY_DATABASE_URL")

// Open opens the connection pool and proves it works, without looking at the
// schema. It is what `hearsay migrate` uses, because a database with no schema
// yet is exactly the case migrating exists for; everything else wants
// [Connect].
//
// Errors never carry the URL: pgx redacts the password from its own parse
// errors, and nothing here puts the string back in.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, ErrNoDatabaseURL
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parsing the database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	return pool, nil
}

// Connect opens the pool and refuses a database whose schema is older than the
// migrations this binary carries: a service verifies, it does not
// migrate. A database newer than the binary is allowed, so that a rollout can
// be migrated forward before every replica has been replaced.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := Open(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := CheckSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
