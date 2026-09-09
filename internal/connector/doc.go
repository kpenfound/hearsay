// Package connector holds the contract every source connector implements and
// the runtime that hosts them.
//
// A connector turns one source — Slack, GitHub, Drive, a coding agent's session
// — into L0 events in the standard shape, and does nothing else. Push where the
// source supports it, poll otherwise. Connectors are plugins: a third party
// ships one plus a source config entry, and its data is searchable and
// distillable with no special handling anywhere else.
//
// See docs/design.md#architecture-and-deployment.
package connector
