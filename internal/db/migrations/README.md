# internal/db/migrations

Schema migrations, applied by `hearsay migrate` as a deploy job, never at service startup.

- One file per migration, `NNNNN_short_description.sql`, never edited once
  merged.
- `-- +goose Up` and `-- +goose Down` in every file. Where a down migration
  would destroy data, say so and fail rather than pretending to reverse.
- One transaction per migration. Statements Postgres will not run in one —
  `CREATE INDEX CONCURRENTLY` above all — get goose's `NO TRANSACTION`
  annotation and a migration to themselves.
- The first migration is `CREATE EXTENSION IF NOT EXISTS vector`.
- `internal/db`'s tests check the naming and the annotations, so a file that
  goose could not version fails the build rather than a deployment.
- Anything running in production changes by expand-and-contract, never in one
  step.
