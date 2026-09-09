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

- Go 1.27.1 or newer (`go.mod` declares the minimum; it is raised deliberately,
  in its own commit).

  ADR-0002 sets the floor at Go 1.24 and says it is raised when something needs
  a newer feature. Nothing does — the floor is the current release because a new
  project should start on a supported one, which is a direction from a person
  and outranks the ADR. The ADR is accepted, so it is not edited; whether its
  floor line gets superseded is a person's call.
- [Dagger](https://dagger.io) v0.21 and a container runtime. Dagger is how
  everything runs: lint, tests, the binary, the image, migrations and the local
  stack. `dagger.json` pins the engine version.
- [golangci-lint](https://golangci-lint.run) v2 for lint and formatting, if you
  want to run it outside Dagger. The module pins the version it runs; match it
  if a lint failure looks like a disagreement.
- Postgres 16 with [pgvector](https://github.com/pgvector/pgvector) — only if
  you want a database Dagger did not start for you.

## Dagger

One tool for everything that runs code, so that CI and a laptop cannot disagree.
`.github/workflows/ci.yml` calls `dagger call check` and does nothing else.

```sh
dagger call check              # lint, tidy, the whole suite, the image: what CI runs
dagger call lint               # go vet, golangci-lint run, golangci-lint fmt --diff
dagger call tidy-check         # fails if `go mod tidy` would change go.mod or go.sum
dagger call unit-test          # go test -race ./...
dagger call integration-test   # the same, tagged `integration`, against pgvector
dagger call test               # both
dagger call build -o ./hearsay # the Linux binary
dagger call image              # the container image hearsay ships in
dagger call postgres up --ports 5432:5432   # a throwaway pgvector database
dagger call migrate --database-url=env:HEARSAY_DATABASE_URL
dagger call dev up             # Postgres plus all four services
dagger functions               # the full list, with the arguments each takes
```

`build` and `image` take `--arch` (default: the engine's) and `--version` to
stamp into the binary.

`dev` has no file watcher: restarting is the reload. Stop it, run it again, and
the rebuild is a cached one. Its database is a throwaway that starts empty every
time — there is no schema yet, so there is nothing to keep; that is the thing to
revisit when the L0 store lands.

`migrate` exits non-zero with "not implemented yet" until goose and the embedded
migrations arrive, and so does the migration step `integration-test` will run
before the tests (ADR-0006). Until then `integration-test` proves the database
itself: it creates the `vector` extension, which fails on a Postgres image
without pgvector.

The Go toolchain and version of the linter come from the repository, not from
this document: the containers read the `go` directive out of `go.mod`, and the
linter version is a constant in `.dagger/main.go`.

### Without Dagger

Dagger needs a container runtime, which is not always available — an agent
session running under a sandbox that denies the Docker socket and the Dagger
registry is the case this project hits. Everything but the integration tests
runs directly:

```sh
go build ./...
go test ./...
go test -race ./...
go vet ./...
golangci-lint run
golangci-lint fmt --diff   # fails if anything is unformatted
golangci-lint fmt          # fixes it
```

The module has no third-party dependencies today, so all of that works on a
fresh clone with no network and no services running — once you have the Go
release `go.mod` asks for. With an older one and the default `GOTOOLCHAIN=auto`,
the first build downloads that toolchain, and only that first build needs the
network.

That is a fallback, not a second gate: the pull request is judged by
`dagger call check`, which runs on every pull request whether or not you could
run it yourself.

## Running it

```sh
go run ./cmd/hearsay help              # every subcommand and what it does
go run ./cmd/hearsay version
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

## Migrations

```sh
go run ./cmd/hearsay migrate up          # apply everything pending
go run ./cmd/hearsay migrate status      # applied and pending
go run ./cmd/hearsay migrate up-to <n>
go run ./cmd/hearsay migrate down        # dev only

dagger call migrate --database-url=env:HEARSAY_DATABASE_URL --action=status
```

All four exit non-zero with "not implemented yet" today: goose, the embedded
migrations and the database connection land with the L0 store. When they do:
migrations are plain SQL in `internal/db/migrations/`, embedded in the binary,
applied by `hearsay migrate` as a deploy job, and **never** automatically on
service startup. See
[ADR-0006](docs/adr/0006-schema-migrations-with-goose.md) before writing one.

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
  read the connection URL from `HEARSAY_DATABASE_URL`. `dagger call
  integration-test` brings the database up and sets it; nothing else does, so a
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

## Pull requests

- Small commits with messages that say why, not what the diff already shows.
- The description says what changed, how you tested it, and any choice you made
  that the issue left open — reviewers rule on those, and stating one costs
  nothing while a silent one costs a round.
- `dagger call check` passes before you push — or, where Dagger cannot run,
  `go test -race ./...` and `golangci-lint run`, and say so in the description.
- Merge `main` into your branch before opening or updating a pull request.
