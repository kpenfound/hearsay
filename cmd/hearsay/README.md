# cmd/hearsay

The one binary Hearsay ships. Each process is a subcommand of it (ADR-0003).

| Subcommand | What it runs |
|---|---|
| `hearsay connectors [--source name]...` | Source connectors. Writes L0 only. |
| `hearsay distiller` | The distiller. L0 to L1. |
| `hearsay assert-worker` | The assertion worker. L1 to L2. |
| `hearsay api` | The read and assert API. |
| `hearsay all` | All four in one process. Local development only. |
| `hearsay migrate up\|status\|up-to <n>\|down` | Schema migrations. Not implemented yet. |
| `hearsay version` | Version, commit and build date. |

Every subcommand that runs something — the services, `all` and `migrate` — takes
`--log-level` and `--log-format`, which also read `HEARSAY_LOG_LEVEL` and
`HEARSAY_LOG_FORMAT`. `version` and `help` take no flags and no arguments, and
say so rather than ignoring what they were given. The `instance` field on every
log line is the hostname, or `HEARSAY_INSTANCE`.

**Belongs here:** flag parsing, building config and dependencies, and calling
`Run`. Nothing else. A subcommand is a thin wrapper over
`internal/service/<name>.Run`; anything you would want to unit-test belongs one
level down, where it can be tested without a process.

The subcommand names are a contract with the Dagger module and the deployment
manifests. Renaming one is a breaking change, not a tidy-up.
