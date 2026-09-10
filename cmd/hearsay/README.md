# cmd/hearsay

The one binary Hearsay ships. Each process is a subcommand of it (ADR-0003).

| Subcommand | What it runs |
|---|---|
| `hearsay connectors [--source name]...` | Source connectors. Writes L0 only. |
| `hearsay distiller` | The distiller. L0 to L1. |
| `hearsay assert-worker` | The assertion worker. L1 to L2. |
| `hearsay api` | The read and assert API. |
| `hearsay all` | All four in one process. Local development only. |
| `hearsay config validate [path]` | Check a configuration repository. Prints every problem, with file and line. |
| `hearsay migrate up\|status\|up-to <n>\|down` | Schema migrations, then exit. `down` refuses without `--i-know`. |
| `hearsay l0 list\|get <id>\|count\|tail` | Inspect the L0 event store. Read-only. |
| `hearsay version` | Version, commit and build date. |

`migrate`, `config` and `l0` take an action word, and flags go on either side of
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
process list. It is on the subcommands that use a database — `migrate`,
`l0`, `all` and `distiller` — and not on the three service subcommands that do
not connect to one yet. `distiller` and `all` refuse to start without it.

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
