# internal/connector

The contract a source connector implements, and the runtime that hosts them.

[docs/connector-contract.md](../../docs/connector-contract.md) is the
specification, and it is normative: a third party implements a connector from
that document without reading this package. The two change together or not at
all.

**Belongs here:** the L0 event type and everything in it — kinds, payload
metadata, identity hints, ACL entries, id derivation and validation — the
connector interface (lifecycle, push vs poll, backfill with a cursor, health),
the source config a connector consumes, the registry and the ingest gate, the
runtime that starts, supervises and shuts connectors down, and a fake connector
for tests. Individual connectors live in subpackages
(`internal/connector/github`, and so on).

**Does not belong here:** anything above L0. A connector emits events; it does
not distill, and it does not decide what an event means. Storing an event is
`internal/l0`; resolving an identity hint to a principal is
`internal/principal`.

The event type lives here rather than in `internal/l0` because the event shape
*is* the connector contract; `internal/l0` is what stores it. Hearsay's own
writers — `assert`, and the audit event every bundle produces — use the same
type.

The interface is the third-party extension point, so a change to it is a
breaking change for anyone shipping a connector.

Three things to know about the runtime before changing it:

- **A connector's `Poll` has exactly one caller**, the goroutine the runtime
  gives it, which is what lets a poller keep its position in memory without
  locking. Anything that would call `Poll` from somewhere else breaks the
  contract rather than the runtime.
- **The backfill cursor is stored before the next call is made**, so an
  interrupted backfill resumes rather than starting again. The store is a
  `CursorStore`, which is `internal/l0`'s `BackfillCursors` in a process and
  `MemoryCursors` in a test; with no store the runtime polls and does not
  backfill, because walking a source's whole history on every restart is worse
  than not walking it.
- **Nothing reaches the sink except through a `Gate`.** The runtime builds one
  per source from the same config the allowlist is built from, so a container
  nobody configured is dropped and counted rather than written.

See [docs/design.md](../../docs/design.md#architecture-and-deployment) and
[ADR-0003](../../docs/adr/0003-one-binary-four-service-subcommands.md) for how
`--source` selects which connectors a process hosts.
