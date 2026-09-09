package providers_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/providers"
)

// One list, read by the process wiring up a registry and by
// `hearsay config validate`: what is in it has to be findable by the name
// configuration calls it.
func TestAllProvidersAreFindableByName(t *testing.T) {
	all := providers.All()
	if len(all) == 0 {
		t.Fatal("All() is empty: this build can call no model at all")
	}
	names := providers.Names()
	if len(names) != len(all) {
		t.Fatalf("Names() has %d entries and All() has %d", len(names), len(all))
	}
	for i, p := range all {
		if names[i] != p.Name() {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], p.Name())
		}
		found, ok := providers.ByName(p.Name())
		if !ok || found.Name() != p.Name() {
			t.Errorf("ByName(%q) did not find it", p.Name())
		}
		if slices.Index(names, p.Name()) != i {
			t.Errorf("two providers are called %q", p.Name())
		}
		// A provider that can back no tier at all is one nothing can use.
		if !slices.ContainsFunc(llm.Tiers, func(tier llm.Tier) bool {
			return llm.CanServe(p.Name(), p.Capabilities(), tier) == nil
		}) {
			t.Errorf("%s can back none of the three tiers", p.Name())
		}
	}
	if _, ok := providers.ByName("nowhere"); ok {
		t.Error(`ByName("nowhere") found a provider`)
	}
}

// Anthropic is the first provider (ADR-0005), and the two completion tiers are
// what it is here for.
func TestAnthropicBacksTheCompletionTiers(t *testing.T) {
	p, ok := providers.ByName(llm.ProviderAnthropic)
	if !ok {
		t.Fatalf("ByName(%q) not found; this build ships %v", llm.ProviderAnthropic, providers.Names())
	}
	for _, tier := range llm.CompletionTiers {
		if err := llm.CanServe(p.Name(), p.Capabilities(), tier); err != nil {
			t.Errorf("anthropic cannot back the %s tier: %v", tier, err)
		}
	}
	if err := llm.CanServe(p.Name(), p.Capabilities(), llm.TierEmbed); err == nil {
		t.Error("anthropic claims an embedding model")
	}
}
