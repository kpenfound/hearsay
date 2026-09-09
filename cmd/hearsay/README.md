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
| `hearsay migrate up\|status\|up-to <n>\|down` | Schema migrations. Not implemented yet. |
| `hearsay version` | Version, commit and build date. |

Every subcommand that runs something — the services, `all`, `config` and
`migrate` — takes `--log-level` and `--log-format`, which also read
`HEARSAY_LOG_LEVEL` and `HEARSAY_LOG_FORMAT`. `version` and `help` take no flags
and no arguments, and say so rather than ignoring what they were given. The
`instance` field on every log line is the hostname, or `HEARSAY_INSTANCE`.

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
