// Package l1 is the document layer: what was said, distilled from L0.
//
// One L1 document per L0 artifact, one row in one table: a common envelope plus
// a per-kind body. Every document carries l0_refs, so any line served to a
// consumer can be followed back to the events it came from. L1 is derived and
// can always be rebuilt from L0.
//
// [Store.Search] is the read over that table design.md calls search(scope,
// query): the embedding of the distillation and the full text of the team's own
// words, each ranked over what the caller may read and fused by reciprocal
// rank. It returns documents, not answers.
//
// See docs/design.md#l1-distilled-documents.
package l1
