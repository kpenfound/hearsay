package principal

import (
	"slices"
	"strings"
)

// Class is an agent's access class: what an agent of that class may read and
// what it may write (docs/design.md#access-control).
//
// The four classes are a chain. Each reads at least what the one before it
// reads and writes at least what it writes, which is what makes intersecting
// two grants the smaller of the two rather than an arbitrary choice
// ([Rights.Intersect]).
type Class string

// The agent classes.
const (
	// ClassObserver reads the scopes it is granted and writes nothing.
	ClassObserver Class = "observer"
	// ClassWorker reads its scopes plus the code entities they link to, and
	// may assert: its stances are proposals.
	ClassWorker Class = "worker"
	// ClassOrchestrator reads several scopes, may assert and may subscribe.
	ClassOrchestrator Class = "orchestrator"
	// ClassSteward may read everything ingested, ratify and merge topics. The
	// class exists so the door is there, closed: ratification is a human
	// action by default, and [AgentRead] takes those two rights away from
	// every agent, whatever its class.
	ClassSteward Class = "steward"
)

// classes is every value [Class] may take, in the order of the table in
// docs/design.md#access-control, which is also least to most.
var classes = []Class{ClassObserver, ClassWorker, ClassOrchestrator, ClassSteward}

// Classes returns every agent class, least privileged first.
func Classes() []Class { return slices.Clone(classes) }

// Valid reports whether c is an agent class.
func (c Class) Valid() bool { return slices.Contains(classes, c) }

// Rights returns what an agent of this class may read and write: the table in
// docs/design.md#access-control, in Go. A class that is not one of the four has
// no rights at all, so a typo that got past validation reads nothing rather
// than everything.
//
// The table's "multiple scopes" for an orchestrator is a fact about which
// scopes it is granted, not about how deeply it reads within one, so it lives
// in [Grant.Scopes] and not here. A worker and an orchestrator therefore have
// the same reach and differ in what they may write.
func (c Class) Rights() Rights {
	switch c {
	case ClassObserver:
		return Rights{Read: ReadScoped}
	case ClassWorker:
		return Rights{Read: ReadScopedCode, Write: WriteAssert}
	case ClassOrchestrator:
		return Rights{Read: ReadScopedCode, Write: WriteAssert | WriteSubscribe}
	case ClassSteward:
		return Rights{Read: ReadAll, Write: WriteAssert | WriteSubscribe | WriteRatify | WriteMerge}
	default:
		return Rights{}
	}
}

// Rights is what a principal may do: how far it may read, and what it may
// write. It says nothing about *which* scopes — that is [Grant.Scopes] — only
// how much of a scope, and how much beyond one.
type Rights struct {
	Read  Read
	Write Write
}

// Intersect returns the rights held by both: the lesser reach, and the writes
// both allow. It is how an agent acting for a human is held to whichever of
// them may do less (docs/design.md#access-control).
func (r Rights) Intersect(other Rights) Rights {
	return Rights{Read: min(r.Read, other.Read), Write: r.Write & other.Write}
}

// Read is how far a principal may read, ordered from least to most so that two
// reaches intersect to the smaller. The values below [ReadAll] are all relative
// to the scopes a principal is granted; [ReadAll] is not, which is why a
// steward is described as reading everything ingested rather than everything in
// its scopes.
type Read uint8

// The read reaches.
const (
	// ReadNone reads nothing.
	ReadNone Read = iota
	// ReadScoped reads the L1 and L3 of the granted scopes.
	ReadScoped
	// ReadScopedCode reads the granted scopes and the code entities they link
	// to.
	ReadScopedCode
	// ReadAll reads everything ingested, whatever the grant says. Only a
	// steward has it.
	ReadAll
)

// String names the reach, for logs and error messages.
func (r Read) String() string {
	switch r {
	case ReadNone:
		return "none"
	case ReadScoped:
		return "scoped"
	case ReadScopedCode:
		return "scoped+code"
	case ReadAll:
		return "all"
	default:
		return "unknown"
	}
}

// Write is the set of writes a principal may make, as a bit set so that two
// principals' writes intersect with an AND.
type Write uint8

// The writes.
const (
	// WriteAssert proposes a stance through the assert API.
	WriteAssert Write = 1 << iota
	// WriteSubscribe subscribes to a scope's changes.
	WriteSubscribe
	// WriteRatify marks a stance ratified. Human by default.
	WriteRatify
	// WriteMerge merges two topics into one. Human by default.
	WriteMerge
)

// writeNames names each bit, lowest first, for [Write.String].
var writeNames = []struct {
	bit  Write
	name string
}{
	{WriteAssert, "assert"},
	{WriteSubscribe, "subscribe"},
	{WriteRatify, "ratify"},
	{WriteMerge, "merge"},
}

// Has reports whether every write in w is allowed.
func (w Write) Has(want Write) bool { return w&want == want }

// String lists the writes, lowest bit first, for logs and error messages. An
// empty set is "none".
func (w Write) String() string {
	if w == 0 {
		return "none"
	}
	names := make([]string, 0, len(writeNames))
	for _, n := range writeNames {
		if w.Has(n.bit) {
			names = append(names, n.name)
		}
	}
	if rest := w &^ allWrites; rest != 0 {
		names = append(names, "unknown")
	}
	return strings.Join(names, ",")
}

// allWrites is every write this version defines.
const allWrites = WriteAssert | WriteSubscribe | WriteRatify | WriteMerge
