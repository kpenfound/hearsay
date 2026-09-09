# internal/service

The four processes, one subpackage each (ADR-0003):

| Package | Subcommand | Does |
|---|---|---|
| `connectors` | `hearsay connectors` | Hosts source connectors. Writes L0 only. |
| `distiller` | `hearsay distiller` | L0 to L1. Stateless, parallel, retryable. |
| `assertworker` | `hearsay assert-worker` | L1 to L2. Serialized per scope. |
| `api` | `hearsay api` | Bundle assembly, MCP and HTTP, audit. |

Every one of them exposes the same entry point:

```go
func Run(ctx context.Context, cfg *config.Config, deps Deps) error
```

`Run` blocks until its context is cancelled or the service fails, and it must
return promptly when cancelled: `hearsay all` composes all four in one process,
so a service that leaks a goroutine or blocks on shutdown hangs local
development. There is a test for that; keep it passing.

**Belongs here:** wiring and the run loop — building the service from its
dependencies, the job handlers it owns, its prompts, and its HTTP surface.

**Does not belong here:** the layer logic itself. A service calls `internal/l0`,
`internal/l1`, `internal/l2`, `internal/bundle` and `internal/queue`; it does
not reimplement them. A service package should stay thin enough that the
interesting code is testable without starting a process.

The four subcommand names are a contract with the Dagger module and the
deployment manifests. `hearsay all` is a development convenience and is never a
deployment target. It is built on `RunAll`, which is a `sync.WaitGroup` and
`errors.Join` rather than the errgroup ADR-0003 names — the module has no
dependencies, and stopping on the first *return* rather than the first *error*
is what `all` actually wants. The reasoning is on `RunAll`'s doc comment.

The four subpackages have no README of their own: what each is for is in its
package comment, next to the code.
