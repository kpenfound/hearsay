# CLAUDE.md

Working notes for an agent (or a person) making a change in this repository.
[CONTRIBUTING.md](CONTRIBUTING.md) has the long version.

## What this is

Hearsay ingests team communication, distills it into four layers that keep their
provenance, and serves scoped context bundles to coding agents. Read
[docs/design.md](docs/design.md) before changing anything structural — it is the
source of truth for the product, and every work item points at a section of it.
The implementation decisions are in [docs/adr/](docs/adr/), and an accepted ADR
is superseded, never edited. The L0 event a connector emits, and the interface it
implements, are specified in
[docs/connector-contract.md](docs/connector-contract.md); that document and
`internal/connector` change together or not at all.

## Commands

Dagger runs everything. The workspace is `dagger.toml`. The tests and the
`go generate` drift check come from the reusable Go module
(`github.com/dagger/go`) it installs; lint, the integration tests, the tidy and
image checks, the binary, the image, migrations and the dev stack are `hearsay`, our own module
in `.dagger/modules/hearsay/main.dang`, written in Dang. Its `go-test-base`
function hands the Go module a container with Postgres attached.
There is no CI workflow: Dagger Cloud runs `dagger check` on every commit.

```sh
dagger check              # every check, in parallel; -l lists them
dagger up                 # Postgres plus all four services
dagger api functions      # the modules; `dagger api call hearsay <fn>` runs one
dagger api call hearsay qa --script '...'     # run a test plan in the shipped image, Postgres beside it
dagger settings go        # the Go module's settings
```

To try a change end to end, use `qa`. It builds the image from the working
tree, binds Postgres, sets `HEARSAY_DATABASE_URL`, writes the script to
`/test.sh`, runs it, and returns the combined output with the exit code on the
last line. `playground` is the interactive equivalent and needs a terminal, so
an agent should reach for `qa`, not `playground`. CONTRIBUTING.md has both.

The Go module's own lint check is switched off in `dagger.toml` (its
golangci-lint is built with an older Go than go.mod asks for); `hearsay:lint`
is the lint gate.

Dagger needs a container runtime. Where there is none — a sandbox that denies
the Docker socket, for instance — everything but the tests that need Postgres runs
directly, and Dagger Cloud is the gate that matters:

```sh
go build ./...            # build everything
go test ./...             # the whole suite; add -race before you push
go vet ./...
golangci-lint run         # lint; config in .golangci.yml
golangci-lint fmt         # format (gofmt + goimports); --diff to only check
go run ./cmd/hearsay help # the subcommands
```

That block runs offline with no services: no *unit* test touches a database or a
model provider. The ones that need Postgres carry the `integration` build tag
and read `HEARSAY_DATABASE_URL`, so a plain `go test ./...` skips them; with a
database of your own, `go test -tags=integration ./...` runs them. Two things
need the network once — the Go toolchain, on a machine whose Go is older than
the release `go.mod` asks for, and the dependencies (the configuration parser
`go.yaml.in/yaml/v3` per ADR-0009, pgx and goose per ADR-0004 and ADR-0006)
until they are in the module cache.

Running a service locally:

```sh
go run ./cmd/hearsay api          # one service
go run ./cmd/hearsay all          # all four in one process, dev only
go run ./cmd/hearsay api --log-level debug --log-format text
go run ./cmd/hearsay api --config ./config   # the configuration repository
go run ./cmd/hearsay config validate ./config
go run ./cmd/hearsay l0 count --database-url=...   # what is in the event store
go run ./cmd/hearsay connectors --source github --listen :8081
```

None of the four services is a stub any more. Every
service reads the configuration repository first when `--config` (or `HEARSAY_CONFIG`) names one,
and refuses to start if it is invalid; configuration is read once, at startup,
and a change to it is a restart (ADR-0009). The format is
[docs/config.md](docs/config.md).

The distiller is not a stub. It reads the L0 change feed, enqueues a `distill`
job per document, and turns each one into an L1 document with a model call
(ADR-0005, ADR-0007), so it needs Postgres — `hearsay distiller` and
`hearsay all` refuse without a database, and with no `--config` they start and
distil nothing, because a configuration is what says there is anything to do.

The connectors service is not a stub either. It builds one connector per
configured source through a registry the binary wires up, polls the pollers on
their own cadence, supervises streams, drives the backfillers through a cursor it keeps in Postgres, walks the ACL
re-syncs a container going private owes from a record it keeps there too
(ADR-0013),
mounts the pushers' handlers under `/hooks/<source>` and answers `/healthz` (the
process is up) and `/readyz` (the database, its schema, and what each source
says about itself) on `--listen` (default `:8081`, and `hearsay all` takes the
flag too). It writes L0, so it refuses without a
database too. GitHub (`internal/connector/github`), Discord
(`internal/connector/discord`), Drive (`internal/connector/drive`), Obsidian
(`internal/connector/obsidian`) and the agent session source
(`internal/connector/agent`, which agents push their own session events to,
authenticated by their API tokens) ship, and their package comments document
settings and deployment constraints. An unregistered type is a startup failure.

The assertion worker is not a stub. The distiller enqueues a serialized `assert`
job, keyed by scope, in the transaction that writes a document whose outcome is
decided, proposed or resolved; the worker reads it with the `assert` tier,
matches topics by reference overlap and then embedding similarity, and appends
stances in `internal/l2`. An `assert` job whose target is an L0 `assertion`
event, which the API enqueues, is an agent's stance and is appended with no
model call. At startup it seeds entities from `code/`, from
the root directories and CODEOWNERS files of the GitHub repositories `code/`
names (`github.Reader`, with the source's token), and from the tracker
hierarchy L0 holds — GitHub sub-issues, `part_of` their parent issue — and
enqueues every such document it has not read. Between startups it follows the
L0 change feed and places each tracker item again when its issue changes. It
needs Postgres and refuses without it.

Migrations are `go run ./cmd/hearsay migrate up|status|up-to <n>|down`, or
`dagger api call hearsay migrate --database-url=...` against a database. They are
plain SQL in `internal/db/migrations/`, embedded with `go:embed`, applied by
`hearsay migrate` as a deploy job and **never** at service startup — read
[ADR-0006](docs/adr/0006-schema-migrations-with-goose.md) before writing one, and
note that a merged migration is never edited. `down` is a development
convenience and refuses without `--i-know`. `hearsay all` migrates for itself,
because a local database is nobody's production; every other command that reads
the database verifies the schema and refuses one that is behind.

The database is `--database-url`, or `HEARSAY_DATABASE_URL`. Prefer the
environment variable: a URL on a command line puts its password in the process
list.

The API is not a stub. It serves `get_bundle`, `resolve`, `stance_history`,
`get_l1`, `get_l0`, `search`, `watch` and `assert` as `POST /v1/<call>` and as MCP tools at `/mcp`,
from one call layer whose bytes both interfaces serve verbatim, on `--listen`
(default `:8080`; `hearsay all` takes `--api-listen`). The caller is named by
`Hearsay-Principal` and authenticated by a bearer token; the agent acting for
them is named by `Hearsay-Agent` and must supply its own token (ADR-0014).
Every call runs within the effective reach — the person's configured scopes
and the agent's, intersected and capped by the agent's class — and every read
is filtered by the person's access lists; every bundle served is an L0 `audit`
event under source `hearsay`. `assert` is an agent's write: only an agent of class `worker` or
above may call it, and it writes an L0 `assertion` event under source `hearsay`
and enqueues the `assert` job that appends its stance under the topic's scope,
with no model call. `watch` is a long poll with an opaque cursor over the L0
change feed: it returns the events on a scope once they are distilled, as
handles with no content, and waits up to 25 seconds when there are none; a
person or an agent of class `orchestrator` or above may call it. The bundle is `internal/bundle`, over the views in `internal/l3`. It
needs Postgres and refuses without it.

`hearsay l0 list|get <id>|count|tail` inspects the event store — what a source
has ingested, an artifact's history, and the change feed the distiller
consumes.

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
under `internal/service`, `internal/llm/anthropic` and `internal/llm/providers`,
and per-source ones later — document themselves in their package comments, and
inherit the parent's README.

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
