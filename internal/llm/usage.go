package llm

import (
	"cmp"
	"slices"
	"sync"
)

// Usage is what one tier, provider and model has cost since the registry was
// built. Every model call in Hearsay goes through one registry method, which is
// what makes cost observable per tier by construction (ADR-0005).
//
// It is a counter rather than a history: the per-call record is the debug log
// line, and the metrics ADR-0008 describes are these numbers once
// internal/telemetry has a meter to report them through.
type Usage struct {
	Tier     Tier
	Provider string
	// Model is the model the provider reported answering with, which is not
	// always the one that was asked for: a provider may resolve an alias.
	Model string
	// Calls is how many calls returned an answer, and Failures how many
	// returned an error after every retry.
	Calls    int
	Failures int
	// InputTokens and OutputTokens are what the answered calls were billed
	// for. An embedding call reports input tokens only.
	InputTokens  int64
	OutputTokens int64
}

// usageKey is what usage is counted per.
type usageKey struct {
	tier     Tier
	provider string
	model    string
}

// usageTable accumulates usage across the tiers of one registry. Every method
// of it is safe to call from several goroutines: a distiller running four jobs
// at once shares one registry.
type usageTable struct {
	mu   sync.Mutex
	rows map[usageKey]*Usage
}

func newUsageTable() *usageTable { return &usageTable{rows: map[usageKey]*Usage{}} }

// row returns the counter for one key, creating it.
func (u *usageTable) row(k usageKey) *Usage {
	row, ok := u.rows[k]
	if !ok {
		row = &Usage{Tier: k.tier, Provider: k.provider, Model: k.model}
		u.rows[k] = row
	}
	return row
}

// answered records a call that returned.
func (u *usageTable) answered(k usageKey, t Tokens) {
	u.mu.Lock()
	defer u.mu.Unlock()
	row := u.row(k)
	row.Calls++
	row.InputTokens += int64(t.Input)
	row.OutputTokens += int64(t.Output)
}

// failed records a call that ran out of attempts.
func (u *usageTable) failed(k usageKey) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.row(k).Failures++
}

// snapshot is every counter, in a stable order: tier as documented, then
// provider, then model.
func (u *usageTable) snapshot() []Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]Usage, 0, len(u.rows))
	for _, row := range u.rows {
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b Usage) int {
		if c := cmp.Compare(slices.Index(Tiers, a.Tier), slices.Index(Tiers, b.Tier)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return cmp.Compare(a.Model, b.Model)
	})
	return out
}
