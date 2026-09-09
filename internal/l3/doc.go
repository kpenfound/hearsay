// Package l3 holds the derived views: what is true right now.
//
// Current stances for an entity including inherited ones, recent L1 activity on
// a scope, open questions, and ownership. Everything here is recomputed from L2
// and is never written directly, so a view can always be thrown away and
// rebuilt.
//
// See docs/design.md#l3-derived-views.
package l3
