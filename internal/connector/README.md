# internal/connector

The contract a source connector implements, and the runtime that hosts them.

**Belongs here:** the connector interface (lifecycle, push vs poll, backfill
with a cursor, health), the source config a connector consumes, the runtime that
starts, supervises and shuts them down, and a fake connector for tests.
Individual connectors live in subpackages (`internal/connector/github`, and so
on).

**Does not belong here:** anything above L0. A connector emits events; it does
not distill, and it does not decide what an event means.

The interface is the third-party extension point, so a change to it is a
breaking change for anyone shipping a connector.

See [docs/design.md](../../docs/design.md#architecture-and-deployment) and
[ADR-0003](../../docs/adr/0003-one-binary-four-service-subcommands.md) for how
`--source` selects which connectors a process hosts.
