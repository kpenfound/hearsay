# 2. Go for language and runtime

- Status: accepted
- Date: 2026-09-08

## Context

Hearsay is four long-running processes (ADR-0003) that are mostly IO: pulling from
Slack, GitHub and Drive, writing to Postgres, calling model providers, and serving a
small read API over HTTP and MCP. The work is concurrent and network-bound rather than
compute-bound. There is no numerical or ML training work in the system; embeddings come
from a provider (ADR-0005) and similarity search happens in Postgres (ADR-0004).

Two deployment facts shape the choice. Hearsay is self-hosted per organization, so the
artifact an operator runs should be a single static binary in a small container with no
runtime to install. And the first consumer is `shed`, in the same ecosystem as Dagger,
whose module SDK and tooling are Go-first.

## Decision

Hearsay is written in Go.

- `go.mod` declares the minimum version the code requires. It starts at **Go 1.24** and
  is raised deliberately, in its own commit, when something needs a newer feature.
- The standard library is the default. `log/slog` for logging (ADR-0008),
  `net/http` for the HTTP server, `context.Context` threaded through every call that
  does IO.
- Dependencies are added when they carry real weight, not for convenience wrappers.
- The build produces one statically linked binary with no cgo, so the container image
  can be built from a minimal base.

## Alternatives considered

- **Rust.** Better at the one thing Hearsay does not need (CPU-bound work) and worse at
  the thing it does (a large surface of ordinary IO glue written quickly). Ecosystem
  coverage for Slack, Google Drive and MCP is thinner, async ergonomics cost reviewer
  time, and the compile-test loop is slower on a codebase this size.
- **TypeScript on Node.** The best SDK coverage of any option, particularly for MCP and
  chat platforms. Rejected on the deployment story: a self-hosted operator gets a
  `node_modules` tree rather than a binary, and the connector processes are exactly the
  kind of long-running concurrent work where Go's model is simpler to reason about than
  a single event loop.
- **Python.** Strongest LLM and embedding ecosystem, and the obvious choice if Hearsay
  did model work itself. It does not: distillation is provider API calls and retrieval
  is SQL. What is left is packaging pain and slower services.
- **Elixir.** A genuinely good fit for the connector supervision and the per-scope
  serialization in ADR-0007. Rejected on scarcity: a smaller contributor pool for an
  open-source project, and no Dagger SDK.

## Consequences

- The four processes ship as one binary (ADR-0003), which is straightforward in Go and
  is part of why the process model is shaped that way.
- Provider SDK gaps are ours to fill. Where a source or model provider has no
  maintained Go SDK, the connector or provider adapter talks to the HTTP API directly.
  The provider abstraction in ADR-0005 already assumes this.
- No cgo means the pgvector client side is pure Go (`pgvector-go` over pgx), and it
  keeps the container image small.
- Structured concurrency has to be imposed by convention rather than by the language:
  every goroutine a process starts is owned by something that can cancel it and wait
  for it. `CONTRIBUTING.md` (#2) carries the rule.
- Testing conventions are Go's: table-driven tests, `testing.T` helpers, no framework.
