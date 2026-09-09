package llm

import "fmt"

// Tier names one of the three things Hearsay uses a model for (ADR-0005). The
// three names are a contract: configuration, the docs and the Dagger module use
// them verbatim, so renaming one is a breaking change.
type Tier string

const (
	// TierDistill is L0 to L1, in the distiller: cheap, high volume, fast.
	TierDistill Tier = "distill"
	// TierAssert is L1 to L2, in the assertion worker: stronger, low volume.
	TierAssert Tier = "assert"
	// TierEmbed is embedding L1 text, on write and on query. Embeddings only:
	// it is served by an [Embedder], never a [Completer].
	TierEmbed Tier = "embed"
)

// Tiers is every tier, in the order they are documented.
var Tiers = []Tier{TierDistill, TierAssert, TierEmbed}

// CompletionTiers are the tiers a [Completer] serves.
var CompletionTiers = []Tier{TierDistill, TierAssert}

// Completion reports whether this tier is served by a [Completer]. Only
// [TierEmbed] is not.
func (t Tier) Completion() bool { return t == TierDistill || t == TierAssert }

// Valid reports whether t is one of the three.
func (t Tier) Valid() bool { return t == TierDistill || t == TierAssert || t == TierEmbed }

// String makes a Tier print as its name.
func (t Tier) String() string { return string(t) }

// ParseTier reads a tier name, which is what configuration holds.
func ParseTier(s string) (Tier, error) {
	t := Tier(s)
	if !t.Valid() {
		return "", fmt.Errorf("no such model tier %q: want %s, %s or %s", s, TierDistill, TierAssert, TierEmbed)
	}
	return t, nil
}
