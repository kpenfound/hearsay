package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"time"

	// stdlib registers pgx as a database/sql driver. goose is a database/sql
	// library, and this is the only place in Hearsay that opens one: every
	// query outside migrations goes through pgx natively (ADR-0004).
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// migrationsFS is the schema, compiled into the binary. Embedding it is what
// makes the binary and the schema it expects impossible to mismatch (ADR-0006):
// there is no directory in the image that can drift from the code.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations is the embedded migrations, rooted where goose looks for them.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		// The path is a constant that go:embed has already resolved, so this
		// cannot happen; returning the whole FS keeps the signature honest
		// without a panic outside main.
		return migrationsFS
	}
	return sub
}

// Migrator applies the embedded migrations to one database. It is what
// `hearsay migrate` runs and nothing else: services verify the schema with
// [CheckSchema] and never migrate (ADR-0006).
type Migrator struct {
	provider *goose.Provider
}

// NewMigrator connects to url and prepares the migrations. The caller closes
// it. log is where goose's own progress goes; a nil logger discards it, which
// is what a test that only cares about the outcome wants.
func NewMigrator(ctx context.Context, url string, log *slog.Logger) (*Migrator, error) {
	if url == "" {
		return nil, ErrNoDatabaseURL
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("connecting to postgres: %w", err), sqlDB.Close())
	}

	// ADR-0006 turns goose's session-level advisory lock on, which is not its
	// default: it is what makes two `migrate up` invocations during a flaky
	// deploy safe, because one applies and the other waits rather than both
	// trying. The lock is held on one session, so the pool is one connection.
	sqlDB.SetMaxOpenConns(1)
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("building the migration lock: %w", err), sqlDB.Close())
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, Migrations(),
		goose.WithSessionLocker(locker),
		goose.WithSlog(log),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("reading the embedded migrations: %w", err), sqlDB.Close())
	}
	return &Migrator{provider: provider}, nil
}

// Close releases the database connection.
func (m *Migrator) Close() error { return m.provider.Close() }

// Applied is one migration this run applied or rolled back.
type Applied struct {
	Version   int64
	Name      string
	Direction string
	Duration  time.Duration
	// Empty reports a migration that was versioned but had nothing to run.
	Empty bool
}

// Status is what the database has done with one embedded migration.
type Status struct {
	Version int64
	Name    string
	Applied bool
	// AppliedAt is zero on a migration that is still pending.
	AppliedAt time.Time
}

// Up applies every pending migration.
func (m *Migrator) Up(ctx context.Context) ([]Applied, error) {
	results, err := m.provider.Up(ctx)
	return applied(results), migrateErr("up", err)
}

// UpTo applies every pending migration no newer than version.
func (m *Migrator) UpTo(ctx context.Context, version int64) ([]Applied, error) {
	results, err := m.provider.UpTo(ctx, version)
	return applied(results), migrateErr("up-to", err)
}

// Down rolls back the most recently applied migration, and only that one.
func (m *Migrator) Down(ctx context.Context) ([]Applied, error) {
	result, err := m.provider.Down(ctx)
	if result == nil {
		return nil, migrateErr("down", err)
	}
	return applied([]*goose.MigrationResult{result}), migrateErr("down", err)
}

// Status is every embedded migration and what the database has done with it,
// oldest first.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	results, err := m.provider.Status(ctx)
	if err != nil {
		return nil, migrateErr("status", err)
	}
	out := make([]Status, 0, len(results))
	for _, r := range results {
		out = append(out, Status{
			Version:   r.Source.Version,
			Name:      filepath.Base(r.Source.Path),
			Applied:   r.State == goose.StateApplied,
			AppliedAt: r.AppliedAt,
		})
	}
	return out, nil
}

// Version is the newest migration the database has applied.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	version, err := m.provider.GetDBVersion(ctx)
	return version, migrateErr("version", err)
}

func applied(results []*goose.MigrationResult) []Applied {
	out := make([]Applied, 0, len(results))
	for _, r := range results {
		out = append(out, Applied{
			Version:   r.Source.Version,
			Name:      filepath.Base(r.Source.Path),
			Direction: r.Direction,
			Duration:  r.Duration,
			Empty:     r.Empty,
		})
	}
	return out
}

// migrateErr names the action and, where goose reports a partial run, the
// migration that failed — which is the thing an operator staring at a
// half-migrated database needs first.
func migrateErr(action string, err error) error {
	if err == nil {
		return nil
	}
	var partial *goose.PartialError
	if errors.As(err, &partial) {
		return fmt.Errorf("migrate %s: %d migration(s) applied, then %s failed: %w",
			action, len(partial.Applied), filepath.Base(partial.Failed.Source.Path), partial.Err)
	}
	return fmt.Errorf("migrate %s: %w", action, err)
}
