# CLAUDE.md

Working notes for an agent (or a person) making a change in this repository.
[CONTRIBUTING.md](CONTRIBUTING.md) has the long version.

## What this is

Hearsay ingests team communication, distills it into four layers that keep their
provenance, and serves scoped context bundles to coding agents. Read
[docs/design.md](docs/design.md) before changing anything structural — it is the
source of truth for the product, and every work item points at a section of it.
The implementation decisions are in [docs/adr/](docs/adr/), and an accepted ADR
is superseded, never edited.

## Commands

Dagger runs everything. The workspace is `dagger.toml`, the module is
`.dagger/main.go`, and the gates are its `+check` functions — `lint`,
`tidy-check`, `unit-test`, `integration-test`, `image-check`. There is no CI
workflow: Dagger Cloud runs `dagger check` on every commit.

```sh
export DAGGER_X_RELEASE=v1.0.0-beta.11   # the release this workspace is on
dagger check              # every check, in parallel; -l lists them
dagger up dev             # Postgres plus all four services
dagger api functions      # everything else that is callable
dagger generate           # after editing .dagger/main.go; commit the result
```

Dagger needs a container runtime. Where there is none — an agent sandbox that
denies the Docker socket, for instance — everything but the integration tests
runs directly, and Dagger Cloud is the gate that matters:

```sh
go build ./...            # build everything
go test ./...             # the whole suite; add -race before you push
go vet ./...
golangci-lint run         # lint; config in .golangci.yml
golangci-lint fmt         # format (gofmt + goimports); --diff to only check
go run ./cmd/hearsay help # the subcommands
```

That block runs offline with no services: the module has no dependencies yet,
and no test touches a database or a model provider. The one exception is the
toolchain itself — on a machine whose Go is older than the release `go.mod` asks
for, the first build downloads it.

Running a service locally:

```sh
go run ./cmd/hearsay api          # one service
go run ./cmd/hearsay all          # all four in one process, dev only
go run ./cmd/hearsay api --log-level debug --log-format text
```

The four services are stubs. Each starts, logs, and exits cleanly on Ctrl-C;
none of them does any work yet.

Migrations are `go run ./cmd/hearsay migrate up|status|up-to <n>|down`, or
`dagger api call migrate --database-url=...` against a database. The command
exists and refuses: goose, the migrations and the database connection land with
the L0 store. Until then it exits non-zero saying so, which is deliberate — see
[ADR-0006](docs/adr/0006-schema-migrations-with-goose.md). The migration step the
`integration-test` check will run before the tests is missing for the same
reason.

## Layout

One binary, four services, layers underneath (ADR-0003).

| Path | Holds |
|---|---|
| `cmd/hearsay` | Subcommands. Flags, config, dependencies, then `Run`. Nothing else. |
| `internal/service` | The four processes, one subpackage each. Every one exposes `Run(ctx, cfg, deps) error`. |
| `internal/l0` … `internal/l3` | The four layers: events, documents, graph, derived views. |
| `internal/bundle` | Context bundle assembly — the primary read. |
| `internal/connector` | The connector contract and runtime. |
| `internal/llm` | Provider abstraction and the `distill`/`assert`/`embed` tiers. |
| `internal/queue` | The Postgres job queue. |
| `internal/db` | Pool, migrations, shared query plumbing. |
| `internal/config`, `internal/principal`, `internal/telemetry`, `internal/version` | Config, identity, logging and tracing, build identity. |

Every directory in that table has a README saying what belongs in it and what
does not. Read the one for the package you are about to touch; it is short, and
it usually names the thing you were about to do wrong. Subpackages — the four
under `internal/service`, and per-source or per-provider ones later — document
themselves in their package comments, and inherit the parent's README.

## Conventions

- **Errors** are returned and wrapped with `fmt.Errorf("...: %w", err)`, and the
  message says what failed, not that it failed. Nothing panics outside `main`.
  Compare with `errors.Is` and `errors.As`, never `==`.
- **Context** is the first parameter of anything that does IO, and is threaded
  through — never stored in a struct, never `context.TODO()` in shipped code.
  Every goroutine is owned by something that can cancel it and wait for it.
- **Tests are table-driven**, with named cases and no framework. Test through the
  package's exported surface. Every bug fix arrives with the test that fails
  without it.
- **No test calls a model provider or a live source.** Model calls in tests go
  through the fake in `internal/llm` with a recorded fixture, checked in.
- **Logging** is `log/slog`, from the context, never a package-level `slog.Info`.
  Never log L0 payloads, L1 text, prompts or completions above `debug`; log ids
  instead. That is access control, not style (ADR-0008).
- **Dependencies** are added when they carry real weight, not for convenience.
  A new one in a pull request is a thing to justify in the description.
- **Commits** are small and say why. The subcommand names, the model tier names
  and the `Run` signature are contracts — changing one is a breaking change and
  needs an ADR, not a rename.
