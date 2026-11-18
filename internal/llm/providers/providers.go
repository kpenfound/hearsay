// Package providers is the list of provider adapters this build ships, in one
// place so that nothing keeps its own copy of it: a process wiring up a
// registry, and `hearsay config validate` deciding whether a configured
// provider exists and can do what the tier needs, read the same list (adding
// a provider is writing one adapter and no other change).
//
// It is separate from internal/llm because an adapter imports the abstraction:
// the list has to live somewhere that can import both.
package providers

import (
	"os"
	"slices"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/anthropic"
	"github.com/kpenfound/hearsay/internal/llm/claudesbx"
	"github.com/kpenfound/hearsay/internal/llm/jev"
)

// Judgments builds Jev, the separate, process-shared typed capability the
// distiller and the assertion worker require; a missing model or key is an
// error. It is intentionally absent from All, Names and ByName.
func Judgments(cfg llm.JudgmentConfig) (llm.Judgments, error) {
	return llm.NewJudgments(cfg, func(c llm.JudgmentConfig, key string) llm.JudgmentProvider { return jev.New(c, key) }, os.Getenv)
}

// All is every provider this build has an adapter for. It is what
// [llm.NewRegistry] is given.
func All() []llm.Provider {
	return []llm.Provider{anthropic.New(), claudesbx.New()}
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
