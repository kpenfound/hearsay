// Package providers is the list of provider adapters this build ships, in one
// place so that nothing keeps its own copy of it: a process wiring up a
// registry, and `hearsay config validate` deciding whether a configured
// provider exists and can do what the tier needs, read the same list (ADR-0005:
// adding a provider is writing one adapter and no other change).
//
// It is separate from internal/llm because an adapter imports the abstraction:
// the list has to live somewhere that can import both.
package providers

import (
	"slices"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/anthropic"
)

// All is every provider this build has an adapter for. It is what
// [llm.NewRegistry] is given.
func All() []llm.Provider {
	return []llm.Provider{anthropic.New()}
}

// Names is what configuration may call a provider, in the order [All] lists
// them.
func Names() []string {
	all := All()
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = p.Name()
	}
	return names
}

// ByName is the adapter configuration named, if this build has it.
func ByName(name string) (llm.Provider, bool) {
	i := slices.IndexFunc(All(), func(p llm.Provider) bool { return p.Name() == name })
	if i < 0 {
		return nil, false
	}
	return All()[i], true
}
