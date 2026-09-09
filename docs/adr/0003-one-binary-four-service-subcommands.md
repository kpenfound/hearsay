# 3. One binary, four service subcommands

- Status: accepted
- Date: 2026-09-08

## Context

`docs/design.md` describes four processes: connectors (write L0 only), the distiller
(L0 to L1), the assertion worker (L1 to L2, serialized per scope), and the API (bundle
assembly, MCP and HTTP, audit). They have different scaling shapes. The distiller is
parallel and high volume, the assertion worker is serialized and low volume, connectors
are IO-bound and long-lived, the API is request-serving.

They also share almost everything below the surface: the same config repo, the same
Postgres schema and row types, the same L0 and L1 vocabulary, the same LLM tier
registry, the same identity mapping. Splitting them into separate repositories or
separate modules would mean versioning that shared vocabulary across a boundary while
it is still changing weekly.

Hearsay is self-hosted per organization. Someone evaluating it should be able to run the
whole thing on a laptop without four terminals and a process manager.

## Decision

One Go module, one binary, `hearsay`, with a subcommand per process.

| Subcommand | Process | Shape |
|---|---|---|
| `hearsay connectors` | Connectors | Long-running. Writes L0 only. `--source` selects which configured connectors to host; the default is all of them. |
| `hearsay distiller` | Distiller | Long-running worker. L0 to L1. Stateless, parallel, retryable. |
| `hearsay assert-worker` | Assertion worker | Long-running worker. L1 to L2. Serialized per scope. |
| `hearsay api` | API | Long-running server. Bundle assembly, MCP and HTTP endpoints, audit. |

These four names are the contract. The repo scaffold (#2) and the Dagger module (#3)
reference them verbatim.

Two more subcommands exist and are not services:

| Subcommand | Purpose |
|---|---|
| `hearsay migrate` | Apply schema migrations and exit (ADR-0006). |
| `hearsay version` | Print version, commit and build date, and exit. |

And one for local development:

| Subcommand | Purpose |
|---|---|
| `hearsay all` | Run all four services in one process, for local dev only. |

Each service is a package under `internal/service/` exposing the same entry point:

```go
// Run blocks until ctx is cancelled or the service fails.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error
```

The subcommand is a thin wrapper that builds config and dependencies and calls `Run`.
`hearsay all` calls all four in one errgroup against one process's dependencies. That is
the reason for the shared signature: `all` must not be a second implementation of
anything, or it will drift from the deployed path and stop being a useful dev tool.

In deployment each service subcommand runs as its own container, from the same image,
differing only in its arguments. `hearsay all` is never a deployment target.

`--source` on `hearsay connectors` lets an operator either run every connector in one
container or split a noisy source into its own. The design calls for one connector per
source; it does not require one process per source, and forcing that would mean a
container per Slack workspace before there is any reason for it.

## Alternatives considered

- **Four binaries in one module.** Four `main` packages, four images, no `all`
  subcommand. Loses the single-process dev mode, and four images to build, tag, scan
  and publish for code that is one dependency graph. The subcommand split gets the same
  deployment isolation from one image.
- **Four repositories, or four modules.** The real cost is the shared vocabulary: the L0
  event shape and the L1 envelope are the connector contract (#4) and are still moving.
  Versioning them across a module boundary now would slow every change to them. If a
  service ever needs its own release cadence, extracting it later is a mechanical move,
  because `Run` is already the seam.
- **One process, always, with internal goroutines.** Simplest to operate and simplest to
  get wrong: the distiller's model calls and the API's latency budget would share a
  process and a memory limit, and a distiller backlog would degrade reads. The design
  explicitly wants these scaled separately.
- **A plugin or worker-pool architecture where one generic `hearsay worker` takes any
  job kind.** Tempting given the shared queue (ADR-0007), but the assertion worker's
  per-scope serialization and the distiller's wide parallelism want different
  concurrency settings and different model tiers. Two named commands say that out loud;
  one generic worker hides it in config.
- **Connectors as separate processes per source, mandatory.** Rejected as above:
  available through `--source`, not imposed.

## Consequences

- One image, one version number, one dependency upgrade. A deployment runs four
  containers from it, plus a `hearsay migrate` job before rollout (ADR-0006).
- A bug in a shared package can take down all four services at once. Accepted: they
  share a database anyway, so the blast radius is already shared.
- `hearsay all` is a supported dev path and needs to keep working. If it breaks, the
  local dev story breaks, so the Dagger `dev` function (#3) exercises it.
- Every service must shut down cleanly on context cancellation, because `all` composes
  them and a service that leaks goroutines or blocks on shutdown will hang the dev
  process. This is the practical enforcement of the concurrency rule in ADR-0002.
- Config is one file for all four services (see #5), with a per-service section where
  something genuinely differs, such as worker concurrency.
- Package layout follows from this: `cmd/hearsay` holds the subcommands and nothing
  else, `internal/service/{connectors,distiller,assertworker,api}` holds the four `Run`
  functions, and the layer packages (`internal/l0`, `internal/l1`, `internal/l2`,
  `internal/bundle`) hold the work they call.
