# Architecture decision records

Decisions that constrain how Hearsay is built. What the system *is* lives in
[`docs/design.md`](../design.md); this directory records why the implementation is
shaped the way it is.

The format, numbering and supersession rules are in [ADR-0001](0001-record-architecture-decisions.md).

| # | Decision | Status | Date |
|---|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions in ADRs | accepted | 2026-09-08 |
| [0002](0002-go-for-language-and-runtime.md) | Go for language and runtime | accepted; version clause superseded by [0010](0010-go-version-floor-tracks-the-current-release.md) | 2026-09-08 |
| [0003](0003-one-binary-four-service-subcommands.md) | One binary, four service subcommands | accepted | 2026-09-08 |
| [0004](0004-one-postgres-for-all-four-layers.md) | One Postgres for all four layers | accepted | 2026-09-08 |
| [0005](0005-llm-provider-abstraction-with-three-model-tiers.md) | LLM provider abstraction with three model tiers | accepted | 2026-09-08 |
| [0006](0006-schema-migrations-with-goose.md) | Schema migrations with goose, applied by `hearsay migrate` | accepted | 2026-09-08 |
| [0007](0007-postgres-backed-job-queue.md) | Postgres-backed job queue with per-key serialization | accepted | 2026-09-08 |
| [0008](0008-observability-slog-and-opentelemetry.md) | Observability: `log/slog` and OpenTelemetry | accepted | 2026-09-08 |
| [0009](0009-configuration-as-a-gitops-directory.md) | Configuration is a GitOps directory of YAML, read at startup | accepted | 2026-09-09 |
| [0010](0010-go-version-floor-tracks-the-current-release.md) | The Go version floor tracks the current release | accepted | 2026-09-09 |

## Names other work depends on

The four service subcommands ([ADR-0003](0003-one-binary-four-service-subcommands.md)):

`hearsay connectors` · `hearsay distiller` · `hearsay assert-worker` · `hearsay api`

plus `hearsay migrate`, `hearsay version`, and `hearsay all` for local development.

The three model tiers ([ADR-0005](0005-llm-provider-abstraction-with-three-model-tiers.md)):

`distill` (L0 to L1, cheap, high volume) · `assert` (L1 to L2, stronger, low volume) · `embed`
