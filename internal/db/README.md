# internal/db

Postgres: the pool, the migrations, and the plumbing the layer packages share.

**Belongs here:** pool construction and configuration (pgx v5 natively, not
`database/sql`), transaction helpers, the startup schema-version check, and the
migrations in [`migrations/`](migrations).

**Does not belong here:** table-specific queries. The L0 store's SQL lives in
`internal/l0`, L1's in `internal/l1`, and so on; this package holds what all of
them need.

**Migrations:** plain SQL, `NNNNN_short_description.sql`, `-- +goose Up` and
`-- +goose Down` in every file, embedded with `go:embed`. Applied by
`hearsay migrate up` as a deploy job before the new services roll — never
automatically at startup. A service whose binary is newer than the database
refuses to start. See
[ADR-0006](../../docs/adr/0006-schema-migrations-with-goose.md).

Tests never build a schema of their own: they run the same migrations, so a
missing one fails CI instead of being papered over by a fixture.
