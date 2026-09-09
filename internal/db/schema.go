package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSchemaBehind is returned by [CheckSchema] when the database has not been
// migrated up to what this binary expects. It is a sentinel so that a service
// can say "run hearsay migrate up" rather than failing later on a missing
// column.
var ErrSchemaBehind = errors.New("the database schema is older than this binary")

// versionTable is goose's own bookkeeping table, at its default name. Reading
// it with pgx rather than through goose is what keeps a service off the
// migration path entirely: verifying costs one query and no *sql.DB.
const versionTable = "goose_db_version"

// undefinedTable is the SQLSTATE Postgres reports for a table that is not
// there, which is what a database nothing has migrated yet looks like.
const undefinedTable = "42P01"

// EmbeddedVersion is the version of the newest migration compiled into this
// binary: the schema version the binary expects a database to be at.
func EmbeddedVersion() (int64, error) {
	versions, err := migrationVersions()
	if err != nil {
		return 0, err
	}
	if len(versions) == 0 {
		return 0, errors.New("no migrations are embedded in this binary")
	}
	return versions[len(versions)-1], nil
}

// SchemaVersion is the newest migration the database has applied, and 0 for a
// database no migration has ever run against.
func SchemaVersion(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var version int64
	err := pool.QueryRow(ctx,
		`SELECT coalesce(max(version_id), 0) FROM `+versionTable+` WHERE is_applied`,
	).Scan(&version)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the schema version: %w", err)
	}
	return version, nil
}

// CheckSchema is the startup check ADR-0006 puts on every process that reads
// the database. It fails when the database is behind the binary and passes when
// it is level or ahead.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	want, err := EmbeddedVersion()
	if err != nil {
		return err
	}
	have, err := SchemaVersion(ctx, pool)
	if err != nil {
		return err
	}
	if have < want {
		return fmt.Errorf("%w: it is at version %d and this binary expects %d; run `hearsay migrate up`", ErrSchemaBehind, have, want)
	}
	return nil
}

// migrationVersions is the version of every embedded migration, ascending. It
// is also where the naming rule `NNNNN_short_description.sql` is enforced: a
// file goose would not version is a file this refuses.
func migrationVersions() ([]int64, error) {
	entries, err := fs.ReadDir(Migrations(), ".")
	if err != nil {
		return nil, fmt.Errorf("reading the embedded migrations: %w", err)
	}
	versions := make([]int64, 0, len(entries))
	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	// fs.ReadDir sorts by name, and the names are zero-padded, so the versions
	// come out ascending. Say so rather than assume it.
	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			return nil, fmt.Errorf("migration versions are not ascending and unique: %d follows %d", versions[i], versions[i-1])
		}
	}
	return versions, nil
}

// migrationVersion is the number a migration file name starts with.
func migrationVersion(name string) (int64, error) {
	base := path.Base(name)
	stem, isSQL := strings.CutSuffix(base, ".sql")
	digits, description, hasDescription := strings.Cut(stem, "_")
	if !isSQL || !hasDescription || description == "" {
		return 0, fmt.Errorf("migration %q is not named NNNNN_short_description.sql", base)
	}
	version, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || version <= 0 {
		return 0, fmt.Errorf("migration %q does not start with a version number", base)
	}
	return version, nil
}
