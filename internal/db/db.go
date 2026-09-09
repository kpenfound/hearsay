package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
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
		// Nothing else holds the pool yet, so this is where it is closed.
		pool.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	return pool, nil
}

// Connect opens the pool and refuses a database whose schema is older than the
// migrations this binary carries (ADR-0006): a service verifies, it does not
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
