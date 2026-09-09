# Contributing to Hearsay

Read [docs/design.md](docs/design.md) first. It is the source of truth for what
Hearsay is, and most disagreements about a change are settled by a paragraph in
it. The implementation decisions behind it — language, process model, storage,
model tiers, migrations, queue, observability — are in
[docs/adr/](docs/adr/).

[CLAUDE.md](CLAUDE.md) is the same material compressed to one page, for an agent
or for a returning contributor who wants the commands and the conventions
without the reasons.

## Prerequisites

- Go, at the release `go.mod` declares. The floor tracks the current Go release
  rather than the oldest one that compiles, and it is raised deliberately, in
  its own commit ([ADR-0010](docs/adr/0010-go-version-floor-tracks-the-current-release.md)).
  With the default `GOTOOLCHAIN=auto`, an older toolchain fetches the right one
  on the first build.

  `go.mod` is the only place a person writes that version, but it is not the
  only file that carries one: raising the floor also re-pins `dagger.lock`, and
  `.dagger/go.mod` is the SDK's, not ours. [Raising the Go
  floor](#raising-the-go-floor) has both.
- [Dagger](https://dagger.io) and a container runtime. Dagger is how everything
  runs: lint, tests, the binary, the image, migrations and the local stack.

  The workspace is on **`v1.0.0-beta.11`**, which Homebrew and winget do not
  carry. Whatever CLI you have will run that release on demand if you tell it
  to, building and caching it the first time:

  ```sh
  export DAGGER_X_RELEASE=v1.0.0-beta.11   # or --x-release=v1.0.0-beta.11 per command
  ```

  `.dagger/dagger-module.toml` pins the same version, and `dagger.toml` is the
  workspace: which modules are installed and how they are configured.
- [golangci-lint](https://golangci-lint.run) v2 for lint and formatting, if you
  want to run it outside Dagger. The module pins the version it runs; match it
  if a lint failure looks like a disagreement.
- Postgres 16 with [pgvector](https://github.com/pgvector/pgvector) — only if
  you want a database Dagger did not start for you.

## Dagger

One tool for everything that runs code, so that CI and a laptop cannot disagree.
There is **no GitHub Actions workflow** in this repository: Dagger Cloud runs the
same checks on every commit, so a workflow would only be a second, slower copy of
`dagger check`.

```sh
dagger check                   # every check below, in parallel; this is the gate
dagger check -l                # list them
dagger check hearsay:lint      # run one
dagger check --failfast        # stop at the first failure instead of seeing all of them
```

The checks are the `+check` functions in `.dagger/main.go`:

| Check | Does |
|---|---|
| `lint` | `go vet`, `golangci-lint run`, `golangci-lint fmt --diff` |
| `tidy-check` | fails if `go mod tidy` would change `go.mod` or `go.sum` |
| `unit-test` | `go test -race ./...` |
| `integration-test` | the same, tagged `integration`, against a pgvector Postgres |
| `image-check` | builds the binary and the container image |

Everything else is an ordinary function, addressed as `<module> <function>` —
the workspace holds the Go SDK as well as `hearsay`, so the module name is not
optional:

```sh
dagger up dev                               # Postgres plus all four services
dagger api call hearsay test                # the unit and integration checks, those two only
dagger api call hearsay build -o ./hearsay  # the Linux binary
dagger api call hearsay image               # the container image hearsay ships in
dagger api call hearsay migrate --database-url=env:HEARSAY_DATABASE_URL
dagger api functions                        # the modules; add a name for its functions
```

`build` and `image` take `--arch` (default: the engine's) and `--version` to
stamp into the binary.

`dev` has no file watcher: restarting is the reload. Stop it, run it again, and
the rebuild is a cached one. Its database is a throwaway that starts empty every
time, and `hearsay all` migrates it on the way up (ADR-0006), so the schema is
there and the rows are not.

`integration-test` runs `hearsay migrate up` between the database and the tests,
from the code under test, so a missing migration fails the check rather than
being papered over by a fixture (ADR-0006). That step is also what proves the
image is the pgvector build: migration 1 creates the `vector` extension, which
stock Postgres cannot.

The Go toolchain and version of the linter come from the repository, not from
this document: the containers read the `go` directive out of `go.mod`, and the
linter version is a constant in `.dagger/main.go`.

### Changing the module

`.dagger/` is a Go module of its own, and its generated bindings
(`dagger.gen.go`, `internal/`) are **committed**, not regenerated at runtime.
After changing `.dagger/main.go`:

```sh
dagger generate    # review the changeset, apply it, and commit the result
```

Dagger does not regenerate those files when it loads the module, so a missing
or stale one is a load failure, not a slow first run. `dagger check` runs the
generators as read-only checks of their own (`dagger-go-sdk:generate`), so
forgetting to commit the changeset fails the same gate a broken test does.

`dagger.toml` pins the Go SDK to a commit rather than tracking its `main`, and
`dagger.lock` pins the container images the module pulls. The SDK's `main` moves
ahead of the released engine: an SDK newer than `v1.0.0-beta.11` loads as a
module that can author nothing, and `dagger generate` then quietly skips
`.dagger` instead of writing the bindings. Move both pins together, with
`dagger update` for the lock, when the workspace moves to a new release.

`.dagger/go.mod` has a `go` directive of its own, currently a minor release
behind the root module's. It is the generated module's, set by the SDK, and it
moves with the SDK pin — leave it alone. Editing it to match `go.mod` is not a
silent mistake: `dagger-go-sdk:generate` fails with `existing go.mod has
unsupported version`.

### Raising the Go floor

The floor tracks the current Go release rather than the oldest one that compiles
([ADR-0010](docs/adr/0010-go-version-floor-tracks-the-current-release.md)). Raise
it in its own commit, doing nothing else, so a new toolchain's stricter vet or
lint lands somewhere it can be read:

1. Edit the `go` directive in `go.mod`. That is the only place a person writes
   the version.
2. Re-pin `dagger.lock`. It records the resolved digest of
   `docker.io/library/golang:<the go directive>`, and nothing derives that from
   `go.mod`. The lock is a record of what a run resolved: it is never pruned,
   and `dagger update` only "refreshes entries already recorded", so it will
   neither add the new pin nor drop the old one. Delete the superseded
   `golang:` line and let a run that actually resolves the new image record it.
3. Confirm it took — `grep golang: dagger.lock` should name the release you just
   moved to, and nothing else.

Step 3 is not optional and no check does it for you. A lock left pinning the
release you moved off passes all seven checks, because the pin only decides
which image a *cold* resolution gets.

### Without Dagger

Dagger needs a container runtime, which is not always available — a sandbox
that denies the Docker socket and the Dagger registry is the case to plan for.
Everything but the integration tests runs directly:

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
golangci-lint run
golangci-lint fmt --diff   # fails if anything is unformatted
golangci-lint fmt          # fixes it
```

The module has three third-party dependencies, each named by an accepted ADR:
`go.yaml.in/yaml/v3` parses the configuration format (ADR-0009),
`github.com/jackc/pgx/v5` is the Postgres driver (ADR-0004), and
`github.com/pressly/goose/v3` applies the migrations (ADR-0006). Once those and
the Go release `go.mod` asks for are in the module cache, all of that works with
no network and no services running — `go test ./...` included, because the tests
that need Postgres carry the `integration` build tag and skip without
`HEARSAY_DATABASE_URL`. With an older Go and the default `GOTOOLCHAIN=auto`, the
first build downloads that toolchain too.

That is a fallback, not a second gate: the pull request is judged by
`dagger check`, which Dagger Cloud runs on every commit whether or not you could
run it yourself.

## Running it

```sh
go run ./cmd/hearsay help              # every subcommand and what it does
go run ./cmd/hearsay version
go run ./cmd/hearsay config validate ./config
go run ./cmd/hearsay api               # one service
go run ./cmd/hearsay all               # all four in one process, dev only
go run ./cmd/hearsay connectors --source github
```

Logging is `--log-level` (debug, info, warn, error) and `--log-format` (json,
text, auto — auto means text when stderr is a terminal). Both also read
`HEARSAY_LOG_LEVEL` and `HEARSAY_LOG_FORMAT`; the flag wins. Logs go to stderr.

Every line carries the three fields ADR-0008 attaches at process start:
`service`, `version` and `instance`. `instance` is the hostname, which is the
replica identity in every container runtime; set `HEARSAY_INSTANCE` where
something knows better.

The four services are stubs: they start, log that they are stubs, and return
when the process is interrupted. That shutdown behaviour is not a placeholder —
`hearsay all` composes all four, so a service that ignores cancellation hangs
local development, and there is a test that keeps it honest.

## Configuration

```sh
go run ./cmd/hearsay config validate ./config   # every problem, with file and line
go run ./cmd/hearsay api --config ./config      # what a service reads at startup
```

Configuration is a directory of YAML applied like GitOps, or a single file that
expands to it. [docs/config.md](docs/config.md) is the schema and
[ADR-0009](docs/adr/0009-configuration-as-a-gitops-directory.md) is why it looks
like that. Two things to know before changing the loader: it is read once, at
startup, so a change to configuration is a restart; and it reports every problem
rather than the first, because fixing configuration one error at a time is what
makes it miserable.

The complete example in docs/config.md is loaded by the tests in
`internal/config`, both as one file and as a directory, and the two have to come
out the same. An example that stops working stops the build, which is the point.

## Migrations

```sh
export HEARSAY_DATABASE_URL=postgres://hearsay:hearsay@localhost:5432/hearsay

go run ./cmd/hearsay migrate up            # apply everything pending
go run ./cmd/hearsay migrate status        # applied and pending
go run ./cmd/hearsay migrate up-to 1
go run ./cmd/hearsay migrate down --i-know # dev only, and it means it

dagger api call hearsay migrate --database-url=env:HEARSAY_DATABASE_URL --action=status
```

Migrations are plain SQL in `internal/db/migrations/`, embedded in the binary
with `go:embed`, applied by `hearsay migrate` as a deploy job, and **never**
automatically on service startup — the one exception is `hearsay all`, which is
local development only and migrates for itself. Read
[ADR-0006](docs/adr/0006-schema-migrations-with-goose.md) before writing one.

The rules that matter when you add one:

- `NNNNN_short_description.sql`, next number, and **never edit a merged
  migration** — fix it forward. A reviewer treats a modified existing migration
  as a defect.
- `-- +goose Up` and `-- +goose Down` in every file. Where a down would destroy
  something it did not create, the down section says so and fails.
- One transaction per migration; anything Postgres will not run in one (
  `CREATE INDEX CONCURRENTLY`) gets goose's `NO TRANSACTION` annotation and a
  migration to itself.
- Anything already running changes by expand-and-contract, never in one step.

Every process that reads the database checks the schema version at startup and
refuses one older than the migrations in its own binary. A database *newer* than
the binary is fine, so a rollout can be migrated forward before every replica
has been replaced.

`--database-url` also reads `HEARSAY_DATABASE_URL`, and that is the one to
prefer: a URL on a command line puts its password in the process list.

## Reading L0

```sh
go run ./cmd/hearsay l0 count                        # events by source and kind
go run ./cmd/hearsay l0 list --source github-acme    # what a source has ingested
go run ./cmd/hearsay l0 list --source github-acme --artifact acme/api#12
go run ./cmd/hearsay l0 get evt:github-acme:acme%2Fapi%2312
go run ./cmd/hearsay l0 tail --source github-acme     # follow the change feed
```

`list` prints what an event is and where it came from and no payload; `get`
prints the whole event, which is what asking for one by id means. `count`
reports what was ever ingested beside what a read returns: L0 is append-only, so
a deletion is a tombstone that hides an event and keeps its row.

`list` and `tail` take the same `--source`, `--kind` and `--artifact`; `get` and
`count` take none of them and say so rather than ignoring one. An artifact's
history is listed oldest first, and `--newest` is the order
[the connector contract](docs/connector-contract.md) puts revisions in, with the
current one first.

## Layout

`cmd/hearsay` holds the subcommands and nothing else. `internal/service/*` holds
the four processes. The layer packages — `internal/l0` through `internal/l3`,
plus `internal/bundle` — hold the work the services call, and the supporting
packages (`internal/connector`, `internal/llm`, `internal/queue`, `internal/db`,
`internal/config`, `internal/principal`, `internal/telemetry`) sit beside them.

**Every top-level package has a README** saying what belongs in it and what does
not. Read the one for the package you are changing, and update it when the
answer changes. That is where the boundaries are written down; nothing else
enforces them. Subpackages — the four under `internal/service`, and the
per-source and per-provider ones to come — say it in their package comment
instead of adding a second README.

## Conventions

### Errors

- Return errors; do not log and return. Wrap with `fmt.Errorf("doing x: %w", err)`
  so the chain reads as a path from the top.
- Error strings are lowercase and unpunctuated, and say what failed rather than
  that something failed.
- Compare with `errors.Is` and `errors.As`. `errorlint` fails the build on `==`
  against a wrapped error.
- Nothing panics outside `main`, and `main` panics only on a programming error.
- A partial failure that a caller can act on is a value, not an error: the
  bundle drops a section rather than failing the read.

### Context

- `ctx context.Context` is the first parameter of every function that does IO or
  can block. It is never stored in a struct.
- No `context.TODO()` in shipped code.
- Every goroutine has an owner that can cancel it and wait for it. Go gives you
  no structured concurrency, so this is a convention and a review point
  (ADR-0002).
- The logger travels in the context. Add fields once, where the work starts,
  with `telemetry.With(ctx, "job_kind", kind)`.

### Tests

- Table-driven, named cases, subtests, no assertion framework:

  ```go
  tests := []struct{ name string; in X; want Y }{...}
  for _, tt := range tests {
      t.Run(tt.name, func(t *testing.T) { ... })
  }
  ```

- Test the exported surface of a package. Reaching into unexported internals to
  make a test easy usually means the boundary is wrong.
- Failure messages say what was called, what came back and what was wanted:
  `t.Errorf("Run(%q) = %v, want nil", args, err)`.
- Use `t.Context()`, `t.Setenv` and `t.TempDir` rather than rolling your own.
- Every bug fix comes with the test that fails without it. Before you push,
  undo the fix and watch the test fail — a regression test that passes either
  way guards nothing.
- Integration tests that need Postgres carry the `integration` build tag, and
  read the connection URL from `HEARSAY_DATABASE_URL`. The `integration-test`
  check brings the database up and sets it; nothing else does, so a
  plain `go test ./...` skips them rather than failing on a machine with no
  Postgres. Tests never build a schema of their own — they run the real
  migrations, so a missing migration fails CI instead of hiding.

### Model calls

**No test calls a model provider.** Model work happens at write time, behind the
tier registry in `internal/llm` (`distill`, `assert`, `embed`), and tests use
the fake with a recorded fixture checked into the repository. Recording a new
fixture is a deliberate act with a real call, done once, reviewed like code. A
test that would silently make a live call is a bug in the test.

### Logging

`log/slog`, taken from the context, never a package-level `slog.Info`. Use the
standard field names in
[ADR-0008](docs/adr/0008-observability-slog-and-opentelemetry.md) so lines from
different services join up, and one `info` line per completed unit of work
rather than a narration of steps.

Never log L0 `payload`, L1 `text` or `raw_text`, prompts, completions,
credentials or webhook signatures above `debug`. Log ids and let whoever is
debugging follow them to the row, where the ACL still applies. This is access
control, not style.

### Dependencies

The standard library is the default (ADR-0002). Add a dependency when it carries
real weight — a database driver, a migration tool — not for a convenience
wrapper. Say why in the pull request. Run `go mod tidy`; CI checks that `go.mod`
and `go.sum` are unchanged by it.

## Changing a decision

The subcommand names, the model tier names, the `Run(ctx, cfg, deps) error`
signature and the L0 event shape are contracts that other work depends on.
Changing one is a design change: write an ADR that supersedes the old one
(`docs/adr/README.md` explains how), and do not edit an accepted ADR to say
something different.

The L0 event shape and the interface a connector implements are specified in
[docs/connector-contract.md](docs/connector-contract.md), which third parties
build against. Adding an optional payload field or a core kind is additive and
only needs that document and `internal/connector` changed together; anything a
connector outside this repository would have to react to needs the ADR.

## Pull requests

- Small commits with messages that say why, not what the diff already shows.
- The description says what changed, how you tested it, and any choice you made
  that the issue left open — reviewers rule on those, and stating one costs
  nothing while a silent one costs a round.
- `dagger check` passes before you push — or, where Dagger cannot run,
  `go test -race ./...` and `golangci-lint run`, and say so in the description.
- Merge `main` into your branch before opening or updating a pull request.
