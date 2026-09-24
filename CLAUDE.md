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
(`github.com/dagger/go`) it installs; lint, the integration tests, the tidy,
image and compose checks, the binary, the image, migrations and the dev stack are `hearsay`, our own module
in `.dagger/modules/hearsay/main.dang`, written in Dang. Its `go-test-base`
function hands the Go module a container with Postgres attached.
There is no CI workflow: Dagger Cloud runs `dagger check` on every commit.

```sh
dagger check              # every check, in parallel; -l lists them
dagger up                 # Postgres plus all four services
dagger api functions      # the modules; `dagger api call hearsay <fn>` runs one
dagger api call hearsay qa --script '...'     # run a test plan in the shipped image, Postgres beside it
dagger api call hearsay compose-smoke         # run deploy/compose in a Docker daemon of its own
dagger settings go        # the Go module's settings
```

To try a change end to end, use `qa`. It builds the image from the working
tree, binds Postgres, sets `HEARSAY_DATABASE_URL`, writes the script to
`/test.sh`, runs it, and returns the combined output with the exit code on the
last line. `playground` is the interactive equivalent and needs a terminal, so
an agent should reach for `qa`, not `playground`. CONTRIBUTING.md has both.

The production deployment is `deploy/compose` (Postgres, a `migrate` job, the
four services from the published image) and its guide is
[docs/deploy.md](docs/deploy.md). `hearsay:compose-check` holds it to that
shape without a Docker daemon, and `compose-smoke` runs it. A service that
comes to read a new environment variable adds it to `deploy/compose/env.example`
and to the guide's env table.

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
go run ./cmd/hearsay init --out ./config/hearsay.yaml   # a new team's config and env file
go run ./cmd/hearsay l0 count --database-url=...   # what is in the event store
go run ./cmd/hearsay connectors --source github --listen :8081
```

None of the four services is a stub any more. Every
service reads the configuration repository first when `--config` (or `HEARSAY_CONFIG`) names one,
and refuses to start if it is invalid; configuration is read once, at startup,
and a change to it is a restart (ADR-0009). The format is
[docs/config.md](docs/config.md).

`hearsay init` writes a new team's single-file `hearsay.yaml` and a mode-0600
env file for the secrets it names, with no database (`internal/onboard`). It
prompts on a terminal, and every prompt has a flag. With `HEARSAY_GITHUB_TOKEN`
in its environment it seeds principals from the repositories' collaborators,
and with `HEARSAY_DISCORD_TOKEN` and `HEARSAY_DRIVE_CREDENTIALS` it adds the
Discord and Drive accounts those sources confirm; it never writes a credential
it did not generate, and refuses to overwrite either file without `--force`.

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

The distiller and assertion worker expose `/healthz` and `/readyz` on
`:8082` and `:8083` by default. Their `--listen` flags and
`HEARSAY_DISTILLER_LISTEN` / `HEARSAY_ASSERT_WORKER_LISTEN` variables move
those listeners; `all` uses `--distiller-listen` and
`--assert-worker-listen` for the same endpoints.

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
follows the feed for GitHub `/hearsay` comment commands too, which the GitHub
connector emits as `command` events (and Hearsay's replies as `github.reply`,
based on `command`, which nothing distils): it enqueues a `gesture:<event id>`
assert job per command as written and per deletion of one, runs the command
under the scope's key through `ApplyGesture` or `ApplyOperation`, records it in
`github_command_replies`, and answers it with one reply comment through a
`github.Replier` with the source's token, which needs issue and pull request
write access. Deleting the command comment undoes it and revises the reply
(ADR-0022). A source configured `read_only: true` opts out: its connector
emits `/hearsay` comments as ordinary messages, the worker enqueues and runs
nothing from it and builds no replier for it, reads none of its reactions as
gestures, and the API registers no Discord command for it. It needs Postgres
and refuses without it.

Topic merge, split and undo are `l2.Operate`: one row appended to the
`l2_topic_operations` ledger, recorded while holding the scope's `assert` serial
key (`queue.Client.Hold`), with no topic or stance row changed
([ADR-0020](docs/adr/0020-topic-operations-are-a-ledger-held-on-the-scope-key.md)).
Every read and the assertion worker's matching follow the ledger: a topic
merged away reads, and is written to, as the topic it went into, and a split's
topic is a topic of its own that gets a row when its first new stance lands
([ADR-0021](docs/adr/0021-reads-follow-the-topic-ledger.md)). `hearsay topics`
and Discord's `/hearsay merge` call `Operate`; GitHub's `/hearsay merge` runs
inside an assert job already holding the key, and calls
`l2.Store.ApplyOperation`, its in-transaction form.

A person's ratify, demote and pin, and the undo of one, are `l2.RecordGesture`
(or `l2.Store.ApplyGesture` inside a job already holding the key): one row in
the `l2_gestures` ledger, keyed by the source L0 event or a CLI-generated event id so a retry
records nothing, recorded under the same serial key and authorized as an
operation is. A ratify or a demote applies to the live stances drawn from the
documents it names and changes no stance row; `l2.Store.Assess`, and so every
bundle and `stance_history`, serves a ratified current stance as `ratified`
and a demoted one as `contested` until a ratification or a newer stance. A pin
writes `l2_pins`
([ADR-0023](docs/adr/0023-human-gestures-are-a-ledger-every-standing-reads.md)).
The assertion worker interprets Discord reaction L0 events and their
tombstones as gestures through this ledger. It resolves the actor and the
containing L1 thread or burst without writing to Discord.
Parsing a source's reaction or command into a request is the connector work
items'. The CLI and Discord's `/hearsay pin` call `RecordGesture` directly.

Discord's `/hearsay pin` and `/hearsay merge` are answered by the API, not the
connector: `api.Interactions` on `POST /discord/<source>/interactions`, for a
Discord source whose settings name `application_id` and `public_key` and
which is not `read_only`. It
verifies the signature, records the command as an L0 `command` event (never
distilled), maps the Discord user to a configured human, applies the command
through `RecordGesture` or `Operate`, and answers ephemerally within Discord's
three seconds, deferring and editing the answer when the work takes longer
([ADR-0024](docs/adr/0024-discord-commands-are-applied-by-the-interaction-adapter.md)).
Merge autocomplete offers the topics `l2.View` lets the person read — the view
`hearsay topics` reads through.

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
`get_l1`, `get_l0`, `get_session`, `search`, `watch` and `assert` as
`POST /v1/<call>` and as MCP tools at `/mcp`,
from one call layer whose bytes both interfaces serve verbatim, on `--listen`
(default `:8080`; `hearsay all` takes `--api-listen`). The caller is named by
`Hearsay-Principal` and authenticated by a bearer token; the agent acting for
them is named by `Hearsay-Agent` and must supply its own token (ADR-0014).
An agent can send `Hearsay-Session` to link bundle audits, handle audits and
assertions to a session artifact; `get_session` follows an assertion id to that
ordered trace (ADR-0017). Bundle audits record the ids served in each trimmed
section; `get_l1`, `get_l0`, `stance_history`, `search` and `resolve` audit
successful handles and refused attempts only when a session is named. Audits
contain ids, never query text or content.
Every call runs within the effective reach — the person's configured scopes
and the agent's, intersected and capped by the agent's class — and every read
is filtered by the person's access lists; every bundle served is an L0 `audit`
event under source `hearsay`. `assert` is an agent's write: only an agent of
class `worker` or
above may call it, and it writes an L0 `assertion` event under source `hearsay`
and enqueues the `assert` job that appends its stance under the topic's scope,
with no model call. `watch` is a long poll with an opaque cursor over the L0
change feed: it returns the events on a scope once they are distilled, as
handles with no content, and waits up to 25 seconds when there are none; a
person or an agent of class `orchestrator` or above may call it. The bundle is
`internal/bundle`, over the views in `internal/l3`. It
needs Postgres and refuses without it.

`hearsay l0 list|get <id>|count|tail` inspects the event store — what a source
has ingested, an artifact's history, and the change feed the distiller
consumes.

`hearsay topics list <scope>|merge <from> <into>|split <topic> --stance <id>... --name <name>|undo <op-id>|ops`
needs `--config` and `--principal <human-id>`, a configured human, never an
agent. It reads what `stance_history` would serve that person — the topics as
the ledger makes them now, filtered by reach and current access lists — and a
topic, stance or operation they may not read is refused exactly as one that
does not exist. `merge`, `split` and `undo` print the id of the operation
`l2.Operate` recorded, and the scope's `ratified_by.principals` decides who may
run them. `ops [--scope] [--since <RFC3339>] [--json]` lists the ledger.

`hearsay gestures ratify|demote|pin|unpin <l1-document-id>|undo <gesture-id>|list`
requires `--config` and `--principal <human-id>`. Writes also accept
`--artifact <source> <artifact-id>` instead of a document id, print the new
gesture id, and use the shared L2 gesture ledger and scope authority. Hidden
targets and ledger records read as missing. `list [--scope] [--since
<RFC3339>] [--json]` filters the ledger by the person's current reach and
evidence access. Topic merge is in `hearsay topics merge`.

`hearsay eval [--since <RFC3339>] [--until <RFC3339>] [--scope <id>] [--json]`
needs `--config`, because standing is computed under the configured authority,
and a database; it takes no principal and writes nothing. It prints the
evaluation metrics over the window (`internal/eval`) as aggregates and ids
only — never a position, a topic name, L1 text or an L0 payload. Time to
ratification replays each topic as the ledger makes it now through `l2.Stand`
over what was recorded before `--until` (`l2.Store.Ratifications`): from its
first moment standing unratified to its first moment standing ratified, by
gesture or by evidence the policy's `ratified_by` ratifies, as median, p90 and
the share still unratified at `--until`. The topic merge and split rate is the
merges and splits in `l2_topic_operations` recorded in the window per topic
the assertion worker opened in it, with undone ones counted and reported
beside, not netted out. The drill-down rate per bundle section, the searches
and resolves that looked beyond the bundle, conflict-flag precision and the
next actions come from L0 (`l0.Store.Timeline`): the session-linked bundle and
handle audits and the agents' `next_action` events, with the bundles served
without a session counted as excluded. `--scope` is the topic scope key for the
first two and the bundle scope for these. `--json` is the stable form.

`hearsay delete --event <l0-id>|--artifact <source> <artifact-id>|--author <identity>`
requires `--reason` and by default previews the forward provenance walk and
writes nothing. `--author` takes a configured principal or a source identity, and
`--json` emits the summary for scripts. `--apply` deletes, and needs `--config`
and `--principal <human-id>`: in one transaction it records the deletion,
redacts the covered L0 payloads in place, writes a `deletion` event under source
`hearsay`, takes out the pins a gesture from a covered event made, and
enqueues a `distill` job for every affected L1 document, which the
running distiller rebuilds or removes
([ADR-0018](docs/adr/0018-operator-deletion-redacts-l0-in-place.md), the one
exception to L0 being append-only). The distiller records each rebuild on the
deletion, and the text L2 read from the deleted content — a superseded stance's
position, a topic's name when nothing surviving supports it — is redacted in
place ([ADR-0019](docs/adr/0019-operator-deletion-redacts-superseded-l2-text.md)).
A gesture whose event is deleted stays in the ledger, holding only ids, and is
out of force for every read.
`hearsay delete list` and `hearsay delete show <id> [--json]` read the records,
with whether each rebuild is complete.

## Layout

One binary, four services, layers underneath (ADR-0003).

| Path | Holds |
|---|---|
| `cmd/hearsay` | Subcommands. Flags, config, dependencies, then `Run`. Nothing else. |
| `internal/service` | The four processes, one subpackage each. Every one exposes `Run(ctx, cfg, deps) error`. |
| `internal/l0` … `internal/l3` | The four layers: events, documents, graph, derived views. |
| `internal/eval` | The evaluation metrics `hearsay eval` prints. |
| `internal/onboard` | The first configuration `hearsay init` writes: `hearsay.yaml` and its env file. |
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
