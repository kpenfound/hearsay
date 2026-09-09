package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/llm"
)

// A configuration that says nothing about models still has the two completion
// tiers: the shipped default lives in the config defaults, so bumping a model
// is one line and running without the section is normal (ADR-0005).
func TestLLMDefaultsWithoutTheSection(t *testing.T) {
	repo, err := config.Load(writeFiles(t, base))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got, want := repo.LLM.Configured(), llm.CompletionTiers; len(got) != len(want) {
		t.Fatalf("Configured() = %v, want %v", got, want)
	}
	distill, ok := repo.LLM.Tier(llm.TierDistill)
	if !ok {
		t.Fatal("the distill tier is not configured")
	}
	if distill.Provider != llm.ProviderAnthropic || distill.Model == "" {
		t.Errorf("the distill tier = %+v, want the shipped default", distill)
	}
	if _, ok := repo.LLM.Tier(llm.TierEmbed); ok {
		t.Error("an embed tier is configured, and no shipped provider can back one")
	}
}

// What the file says is merged over the defaults, field by field: a tier that
// names a model keeps the provider it did not name.
func TestLLMMergesOverTheDefaults(t *testing.T) {
	repo, err := config.Load(writeFiles(t, with(map[string]string{
		"llm/tiers.yaml": "" +
			"tiers:\n" +
			"  distill:\n    max_tokens: 4096\n    timeout: 90s\n" +
			"  assert:\n    model: claude-opus-5\n    temperature: 0\n    max_retries: -1\n    backoff: 2s\n",
	})))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	defaults := llm.Default()

	distill, _ := repo.LLM.Tier(llm.TierDistill)
	if distill.MaxTokens != 4096 || distill.Timeout != 90*time.Second {
		t.Errorf("the distill tier = %+v, want what the file says", distill)
	}
	if want := defaults.Tiers[llm.TierDistill]; distill.Provider != want.Provider || distill.Model != want.Model {
		t.Errorf("the distill tier = %+v, want the default provider and model kept", distill)
	}

	assert, _ := repo.LLM.Tier(llm.TierAssert)
	if assert.Model != "claude-opus-5" || assert.Temperature == nil || *assert.Temperature != 0 {
		t.Errorf("the assert tier = %+v, want the model and temperature from the file", assert)
	}
	if assert.Provider != llm.ProviderAnthropic || assert.MaxTokens != defaults.Tiers[llm.TierAssert].MaxTokens {
		t.Errorf("the assert tier = %+v, want the rest of the default kept", assert)
	}
	if assert.Retries() != 0 || assert.Backoff != 2*time.Second {
		t.Errorf("the assert tier = %+v, want retries off and a two-second backoff", assert)
	}
}

// The two forms are the same configuration here too.
func TestLLMSingleFileFormIsTheSame(t *testing.T) {
	dir, err := config.Load(writeFiles(t, with(map[string]string{
		"llm/tiers.yaml": "tiers:\n  assert:\n    model: claude-opus-5\n",
	})))
	if err != nil {
		t.Fatalf("Load(the directory form) = %v", err)
	}
	single := writeFiles(t, map[string]string{"hearsay.yaml": "" +
		"sources:\n  - id: github\n    type: github\n    containers: [acme/api]\n" +
		"scopes:\n  - id: api\n    sources: [github]\n" +
		"llm:\n  tiers:\n    assert:\n      model: claude-opus-5\n"})
	file, err := config.Load(single)
	if err != nil {
		t.Fatalf("Load(the single-file form) = %v", err)
	}
	one, _ := dir.LLM.Tier(llm.TierAssert)
	two, _ := file.LLM.Tier(llm.TierAssert)
	if one != two {
		t.Errorf("the directory form gives %+v and the single file gives %+v", one, two)
	}
}

func TestLLMProblems(t *testing.T) {
	for _, tt := range []struct {
		name string
		file string
		want []string
	}{
		{
			name: "a tier that is not one of the three",
			file: "tiers:\n  summarize:\n    model: m\n",
			// The line is the tier's first field, which is where an object in
			// a list is located too.
			want: []string{`llm/tiers.yaml:3: model tier "summarize": no such model tier "summarize"`},
		},
		{
			name: "a key that is not a field",
			file: "tiers:\n  distill:\n    tokens: 10\n",
			want: []string{`llm/tiers.yaml:3: no such field "tokens" in a model tier`},
		},
		{
			name: "a key that is not a field of the section",
			file: "tears:\n  distill:\n    max_tokens: 10\n",
			want: []string{`llm/tiers.yaml:1: no such field "tears"`},
		},
		{
			name: "a provider this build has no adapter for",
			file: "tiers:\n  distill:\n    provider: openai\n    model: gpt-5\n",
			want: []string{`llm/tiers.yaml:3: model tier "distill": provider: no such provider "openai"`},
		},
		{
			name: "a provider that cannot do what the tier needs",
			file: "tiers:\n  embed:\n    provider: anthropic\n    model: claude-haiku-4-5-20251001\n    dimensions: 1024\n",
			want: []string{`model tier "embed": provider: anthropic has no embedding model`},
		},
		{
			name: "an embed tier with no width",
			file: "tiers:\n  embed:\n    provider: anthropic\n    model: m\n",
			want: []string{`model tier "embed": dimensions is required on the embed tier`},
		},
		{
			name: "a width on a tier that answers with text",
			file: "tiers:\n  distill:\n    dimensions: 1024\n",
			want: []string{`model tier "distill": dimensions is set on the distill tier`},
		},
		{
			name: "a credential where the name of its variable belongs",
			file: "tiers:\n  distill:\n    api_key_env: sk-ant-nope\n",
			want: []string{`model tier "distill": api_key_env "sk-ant-nope" is not the name of an environment variable`},
		},
		{
			name: "a timeout that is not a duration",
			file: "tiers:\n  distill:\n    timeout: soon\n",
			want: []string{`model tier "distill": timeout: "soon" is not a duration`},
		},
		{
			name: "a temperature outside the range",
			file: "tiers:\n  distill:\n    temperature: 2\n",
			want: []string{`model tier "distill": temperature is 2`},
		},
		{
			name: "a tier that is not a mapping",
			file: "tiers:\n  distill: claude-haiku-4-5-20251001\n",
			want: []string{`llm/tiers.yaml:2: want a mapping of provider, model and the tier's parameters`},
		},
		{
			name: "a section with no tiers in it",
			file: "tiers: {}\n",
			want: []string{"llm/tiers.yaml:1: llm: tiers: no model tiers are configured here"},
		},
		{
			name: "every problem, not the first",
			file: "tiers:\n  distill:\n    provider: openai\n    model: m\n  assert:\n    timeout: whenever\n",
			want: []string{`no such provider "openai"`, `"whenever" is not a duration`},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeFiles(t, with(map[string]string{"llm/tiers.yaml": tt.file})))
			var invalid *config.InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("Load() = %v, want an *InvalidError", err)
			}
			got := make([]string, len(invalid.Problems))
			for i, p := range invalid.Problems {
				got[i] = p.Error()
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Load() reported %d problems, want %d:\n%s", len(got), len(tt.want), strings.Join(got, "\n"))
			}
			for _, want := range tt.want {
				if !containsSubstring(got, want) {
					t.Errorf("no problem contains %q; got:\n%s", want, strings.Join(got, "\n"))
				}
			}
		})
	}
}

// One tier configured in two files is a collision, the way two objects sharing
// an id are: nothing says which of them wins.
func TestLLMTierConfiguredTwice(t *testing.T) {
	_, err := config.Load(writeFiles(t, with(map[string]string{
		"llm/a.yaml": "tiers:\n  distill:\n    max_tokens: 1024\n",
		"llm/b.yaml": "tiers:\n  distill:\n    max_tokens: 2048\n",
	})))
	if err == nil {
		t.Fatal("Load() = nil, want the collision")
	}
	if !strings.Contains(err.Error(), "already configured") {
		t.Errorf("Load() = %q, want it to say where the tier was already configured", err)
	}
}

// The configuration the loader produces is what the registry builds from: the
// two cannot disagree about what a valid tier is.
func TestLoadedConfigurationBuildsARegistry(t *testing.T) {
	repo, err := config.Load(writeFiles(t, with(map[string]string{
		"llm/tiers.yaml": "tiers:\n  distill:\n    max_tokens: 512\n",
	})))
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	registry, err := llm.NewFake(repo.LLM, llm.NewFixtures())
	if err != nil {
		t.Fatalf("NewFake(the loaded configuration) = %v", err)
	}
	if _, err := registry.Completer(llm.TierDistill); err != nil {
		t.Errorf("Completer(distill) = %v", err)
	}
}
