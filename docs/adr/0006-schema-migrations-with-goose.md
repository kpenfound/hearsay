# 6. Schema migrations with goose, applied by `hearsay migrate`

- Status: accepted
- Date: 2026-09-08

## Context

Everything is in one Postgres database (ADR-0004), so the schema is the interface
between all four services and the thing most likely to break a deployment. Four
containers from one image (ADR-0003) start in no particular order against one database,
which rules out any scheme where a service migrates on its own startup.

Some migrations are not DDL. Backfilling a new L1 column from existing rows, or
re-embedding L1 after an embedding model change (ADR-0005), is a data migration that
needs application code and cannot be a single SQL statement.

The same migrations have to run in three places: a developer's local Postgres, the
throwaway Postgres the Dagger integration test brings up (#3), and a real deployment.
They must be the same set, applied the same way, or CI stops being evidence.

## Decision

**Tool: [goose](https://github.com/pressly/goose)**, used as a library, with migrations
written as plain SQL.

- Migrations live in `internal/db/migrations/`, named
  `NNNNN_short_description.sql`, embedded into the binary with `go:embed`. The binary
  and the schema it expects ship together and cannot be mismatched.
- `-- +goose Up` and `-- +goose Down` in every file. Down migrations are written where
  they are cheap and honest; where a change destroys data, the down section says so and
  fails rather than pretending to reverse. Down migrations are a development
  convenience. Recovery in production is a restore, not a down migration.
- Each migration runs in a transaction. The exceptions are statements Postgres will not
  run in one, principally `CREATE INDEX CONCURRENTLY`, which use goose's
  `NO TRANSACTION` annotation and are kept alone in their own migration.
- The first migration is `CREATE EXTENSION IF NOT EXISTS vector`.
- Go migrations are available for data backfills that need application code, and are the
  exception rather than the norm.

**Application: `hearsay migrate`** (ADR-0003), never automatically on service startup.

```
hearsay migrate up            # apply everything pending
hearsay migrate status        # what is applied, what is pending
hearsay migrate up-to <N>     # for deployments that roll forward in steps
hearsay migrate down          # dev only; refuses without --i-know
```

`hearsay migrate` enables goose's session-level advisory lock, so two concurrent
`migrate up` invocations are safe: one applies, the other waits. It is not goose's
default, and turning it on is what makes a re-run during a flaky deploy harmless.

**Services verify, they do not migrate.** At startup each service reads the goose
version table and refuses to start if the database version is *older* than the highest
migration embedded in the binary. A database *newer* than the binary is allowed, so that
a rollout can be forward-migrated before every replica has been replaced.

**How it runs in each environment:**

- **Dev**: `dagger call migrate` against the local database, and `hearsay all` runs
  `migrate up` for itself before starting the services, because a local database is
  nobody's production.
- **CI**: the integration test function brings up the pgvector Postgres, runs
  `hearsay migrate up` from the code under test, and then runs tests. Tests never build
  a schema of their own, so a missing migration fails CI rather than being papered over
  by test fixtures.
- **Deployment**: a `hearsay migrate up` job from the new image runs to completion
  before the new service containers roll. In compose this is a one-shot service the
  others depend on; in Helm (#29) it is a pre-upgrade hook job.

**Expand-and-contract is the rule for anything running.** A change that would break the
currently-deployed binary is split across releases: add the new column and write both,
release, backfill, stop writing the old, drop it in a later release. This is what makes
"migrate before rolling" safe.

## Alternatives considered

- **[golang-migrate](https://github.com/golang-migrate/migrate).** The most widely used
  option and a reasonable choice. Rejected on two points: its dirty-state model, where a
  failed migration leaves the version table marked dirty and requires a human to force a
  version before anything can proceed, is a bad property for an operator's first upgrade;
  and it has no path for data migrations that need Go.
- **[Atlas](https://atlasgo.io).** Declarative schema, diffing, linting, genuinely good
  tooling, and it understands pgvector. Rejected as too much machinery for a schema this
  young: declarative diffing is most valuable when the schema is large and stable, and
  the parts that matter most here (versioned migrations, an advisory lock) are what
  goose already does. Worth revisiting if hand-written migrations become a burden.
- **tern.** Clean, pgx-native, and would fit ADR-0004's driver choice neatly. Smaller
  community and no embedded-FS-plus-Go-migrations story as strong as goose's.
- **Migrations in the ORM.** There is no ORM. Queries are SQL against pgx, and the
  hybrid retrieval and recursive CTEs in ADR-0004 are not code an ORM would generate.
- **Auto-migrate on service startup.** Removes a deployment step, and races four
  containers against one database. Even with an advisory lock, it means a rollback of
  the image does not roll back the schema, and the first thing a new version does is
  mutate production before anyone has looked at it. Rejected.
- **A `migrations/` directory read from disk at runtime.** Means the container image
  carries a directory that can drift from the binary, and an operator can apply a
  migration set that does not match what is running. Embedding removes the failure mode.

## Consequences

- The binary knows its schema version, so a mismatched deploy fails loudly at startup
  with a clear message instead of failing later on a missing column.
- Migration files are append-only once merged. Editing an applied migration is not
  allowed; fix it forward with a new one. Reviewers should treat a modified existing
  migration as a defect.
- Migration numbers collide between concurrent branches. The second to merge renumbers,
  the same rule as ADR-0001.
- `hearsay migrate` is a fifth subcommand alongside the four service commands. It is not
  a fifth process: it runs and exits. ADR-0003 lists it as such.
- The integration test's Postgres must be a pgvector image, or migration 1 fails. Same
  constraint as ADR-0004.
- Expand-and-contract means some schema changes take two releases. That cost is
  deliberate and buys zero-downtime upgrades for an operator who is running this against
  their team's real communication.
