// Package l2 is the graph: what it means.
//
// Entities, topics, stances and the edges between them, built from L1 by the
// assertion worker and correctable by hand. Stances are superseded, never
// overwritten, so the record of how a decision changed survives. Writes are
// serialized per scope (ADR-0007).
//
// See docs/design.md#l2-the-graph.
package l2
