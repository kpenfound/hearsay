// Package connector holds the contract every source connector implements and
// the runtime that hosts them.
//
// A connector turns one source — Slack, GitHub, Drive, a coding agent's session
// — into L0 events in the standard shape, and does nothing else. Push where the
// source supports it, poll otherwise. Connectors are plugins: a third party
// ships one plus a source config entry, and its data is searchable and
// distillable with no special handling anywhere else.
//
// The pieces are [Event] and what it is made of, [Connector] with the [Poller],
// [Pusher] and [Backfiller] modes, [SourceConfig], the [Registry] that builds a
// connector for a source, the [Gate] that decides what may be written, and
// [Fake] for tests.
//
// docs/connector-contract.md is the specification this package implements, and
// it is what a third party writes a connector from. The two change together.
//
// See docs/design.md#architecture-and-deployment.
package connector
