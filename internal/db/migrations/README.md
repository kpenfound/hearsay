# internal/db/migrations

Schema migrations, applied by `hearsay migrate` (ADR-0006). Empty until the L0
store lands.

- One file per migration, `NNNNN_short_description.sql`, never edited once
  merged.
- `-- +goose Up` and `-- +goose Down` in every file. Where a down migration
  would destroy data, say so and fail rather than pretending to reverse.
- One transaction per migration. Statements Postgres will not run in one —
  `CREATE INDEX CONCURRENTLY` above all — get goose's `NO TRANSACTION`
  annotation and a migration to themselves.
- The first migration is `CREATE EXTENSION IF NOT EXISTS vector`.
- Anything running in production changes by expand-and-contract, never in one
  step.

See [ADR-0006](../../../docs/adr/0006-schema-migrations-with-goose.md).
