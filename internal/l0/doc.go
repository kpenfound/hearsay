// Package l0 is the event layer: what happened, as the source reported it.
//
// An event is {id, source, native_id, kind, time, payload, acl}, and its type
// is [github.com/kpenfound/hearsay/internal/connector.Event]: the shape is the
// connector contract, and this package is what stores it. L0 is append-only;
// deletion writes a tombstone and then walks provenance forward. Connectors
// write here and nowhere else.
//
// See docs/design.md#l0-events and docs/connector-contract.md. Nothing in this
// package may call a model.
package l0
