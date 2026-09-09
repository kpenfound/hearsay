// Package bundle assembles the context bundle, Hearsay's primary read.
//
// Built deterministically from L2 and L3 for a scope and a principal: pointers
// plus one line of distillation per pointer, never raw content, every line
// carrying an id the consumer can follow. The same scope and principal with no
// new events must produce the same bundle, and the same bundle regardless of
// which interface asked.
//
// See docs/design.md#the-context-bundle.
package bundle
