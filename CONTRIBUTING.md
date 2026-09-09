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
- [golangci-lint](https://golangci-lint.run) v2 for lint and formatting. CI pins
  the version it runs; match it if a lint failure looks like a disagreement.
- Postgres 16 with [pgvector](https://github.com/pgvector/pgvector) — for the
  database work that is coming. Nothing in the repository needs it yet.

## Build, test, lint

```sh
go build ./...
go test ./...
go test -race ./...   # what CI runs; run it before you push
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

CI runs those same commands on every pull request. Issue #35 replaces the
workflow with calls into a Dagger module so CI and local development run
literally the same code; until it lands, `.github/workflows/ci.yml` runs the
commands above directly.

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
- Integration tests that need Postgres run against a real database brought up by
  the test harness, applying the real migrations. Tests never build a schema of
  their own, so a missing migration fails CI instead of hiding.

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
- `go test -race ./...` and `golangci-lint run` pass before you push.
- Merge `main` into your branch before opening or updating a pull request.
