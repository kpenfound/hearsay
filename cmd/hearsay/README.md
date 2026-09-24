# cmd/hearsay

The one binary Hearsay ships. Each process is a subcommand of it (ADR-0003).

| Subcommand | What it runs |
|---|---|
| `hearsay connectors [--source name]...` | Source connectors. Writes L0 only. Serves webhooks and health on `--listen`. |
| `hearsay distiller` | The distiller. L0 to L1. |
| `hearsay assert-worker` | The assertion worker. L1 to L2. |
| `hearsay api` | The read and assert API. |
| `hearsay all` | All four in one process. Local development only. |
| `hearsay config validate [path]` | Check a configuration repository. Prints every problem, with file and line. |
| `hearsay migrate up\|status\|up-to <n>\|down` | Schema migrations, then exit. `down` refuses without `--i-know`. |
| `hearsay l0 list\|get <id>\|count\|tail` | Inspect the L0 event store. Read-only. |
| `hearsay aliases list\|confirm <entity> <name>\|reject <entity> <name>` | List candidates and decide their names as a configured human. |
| `hearsay delete --event <l0-id>\|--artifact <source> <artifact-id>\|--author <identity> [--apply]` | Preview deletion provenance; with `--apply`, redact L0 and re-distill what depended on it. |
| `hearsay version` | Version, commit and build date. |

`hearsay aliases` requires `--config` and `--principal <human-id>`. Its list shows
entity, name, state and vote count only where that human may read every current
evidence document. Confirm and reject take the entity id and name shown by list.

`hearsay delete` takes exactly one selector and requires `--reason` even for a
preview. `--artifact` covers every revision. `--author` takes a configured
principal (with `--config`) or `source:native-id` (or `source:@handle`); a
principal expands to all configured source identities. The summary names the
covered L0 events, dependent L1 documents and L2 stances, topics, alias
candidates and pins. `--json` prints the same walk as a JSON object. The
preview is the default and writes nothing.

`--apply` deletes, and needs `--config` and `--principal <human-id>`, a human
principal in that configuration. A missing flag, an unknown id or an agent is
refused before the database is opened, and `--principal` without `--apply` is
an error. One transaction records the deletion with the principal as operator,
redacts the covered L0 events in place, writes a `deletion` event under source
`hearsay`, and enqueues a `distill` job for every L1 document the walk found
([ADR-0018](../../docs/adr/0018-operator-deletion-redacts-l0-in-place.md)).
It prints the deletion id, the events redacted and the documents queued, or
with `--json` the same as an object. Events an earlier deletion already covered
are left alone. `hearsay l0 get` on a deleted event fails naming the deletion
and operator, and prints no content.

`migrate`, `config`, `l0` and `aliases` take an action word, and flags go on either side of
it and after its argument: `hearsay l0 get <id> --database-url x` and
`hearsay l0 --database-url x get <id>` are the same command. Each action reads
its own flags — `l0 list` and `l0 tail` take `--source`, `--kind` and
`--artifact`, `migrate down` takes `--i-know` — and a flag an action does not
read is an error rather than a no-op, the same way a stray argument is: a filter
silently dropped is a wrong answer nobody has a reason to doubt.

Every subcommand that runs something — the services, `all`, `config` and
`migrate` — takes `--log-level` and `--log-format`, which also read
`HEARSAY_LOG_LEVEL` and `HEARSAY_LOG_FORMAT`. `version` and `help` take no flags
and no arguments, and say so rather than ignoring what they were given. The
`instance` field on every log line is the hostname, or `HEARSAY_INSTANCE`.

`--database-url` points at Postgres, and also reads `HEARSAY_DATABASE_URL`,
which is the one to prefer: a URL on a command line puts its password in the
process list. It is on every subcommand that uses a database — `migrate`, `l0`, `aliases`, `delete`,
`all` and the four services — and every one of them refuses to start without
it.

`--listen` is on `connectors` and on `all`, which runs it, and also reads
`HEARSAY_LISTEN`. It is where the push connectors' handlers are mounted, under
`/hooks/<source>`, and where the connectors service answers `/healthz` and
`/readyz`
([ADR-0008](../../docs/adr/0008-observability-slog-and-opentelemetry.md)). The
default is `:8081`, the port after the API's 8080; a machine where something
else holds that port is why `all` takes the flag too.

`hearsay api` takes `--listen` as well, for its own address, and reads
`HEARSAY_API_LISTEN` rather than `HEARSAY_LISTEN`, because `all` runs both
services and one variable cannot name two ports. `all` spells it `--api-listen`.
The default is `:8080`. The API serves its calls as `POST /v1/<call>`, the same
calls as MCP tools at `/mcp`, and `/healthz` and `/readyz`.

`--config` points at the configuration repository, and also reads
`HEARSAY_CONFIG`. A service loads it before it starts and refuses to start if it
is invalid; started without one it warns and runs empty, which is what
evaluating the binary looks like. Configuration is read once
([ADR-0009](../../docs/adr/0009-configuration-as-a-gitops-directory.md)), so
this package has no reload path to maintain.

**Belongs here:** flag parsing, building config and dependencies, and calling
`Run`. Nothing else. A subcommand is a thin wrapper over
`internal/service/<name>.Run`; anything you would want to unit-test belongs one
level down, where it can be tested without a process.

The subcommand names are a contract with the Dagger module and the deployment
manifests. Renaming one is a breaking change, not a tidy-up.
