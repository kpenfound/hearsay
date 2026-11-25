package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/config/configtest"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/claudesbx/claudesbxtest"
	"github.com/kpenfound/hearsay/internal/llm/providers"
)

func claudeSbxConfig(t *testing.T, tiers string) string {
	t.Helper()
	return configtest.Write(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":     "id: api\nsources: [github]\n",
		"llm/tiers.yaml":      tiers,
	})
}

const claudeSbxTiers = "tiers:\n  distill:\n    provider: claude-sbx\n    model: claude-haiku-5-5\n    max_tokens: 1500\n" +
	"  assert:\n    provider: claude-sbx\n    model: claude-sonnet-5-5\n"

func TestConfigValidateAcceptsClaudeSbxWithNoSbx(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	path := claudeSbxConfig(t, claudeSbxTiers)

	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"config", "validate", path}, &stdout, &stderr); err != nil {
		t.Fatalf("config validate = %v, want a valid configuration", err)
	}
	for _, want := range []string{"distill=claude-sbx/claude-haiku-5-5", "assert=claude-sbx/claude-sonnet-5-5"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the summary = %q, want it to list %s", stdout.String(), want)
		}
	}
}

func TestClaudeSbxRefusalsAreTheSameInValidationAndTheRegistry(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, tt := range []struct {
		name  string
		tier  string
		yaml  string
		tc    llm.TierConfig
		wants []string
	}{
		{"temperature", "distill", "    temperature: 0.2\n", llm.TierConfig{Temperature: new(0.2)}, []string{"distill", "claude-sbx", "temperature"}},
		{"base_url", "assert", "    base_url: https://gateway.example\n", llm.TierConfig{BaseURL: "https://gateway.example"}, []string{"assert", "claude-sbx", "base_url"}},
		{"api_key_env", "distill", "    api_key_env: MY_KEY\n", llm.TierConfig{APIKeyEnv: "MY_KEY"}, []string{"distill", "claude-sbx", "api_key_env"}},
		{"embed", "embed", "    dimensions: 8\n", llm.TierConfig{Dimensions: 8}, []string{"embed", "claude-sbx", "no embedding model"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := "tiers:\n  " + tt.tier + ":\n    provider: claude-sbx\n    model: m\n" + tt.yaml
			_, loadErr := config.Load(claudeSbxConfig(t, file))
			tt.tc.Provider, tt.tc.Model = llm.ProviderClaudeSbx, "m"
			_, regErr := llm.NewRegistry(llm.Config{Tiers: map[llm.Tier]llm.TierConfig{llm.Tier(tt.tier): tt.tc}}, providers.All())
			for name, err := range map[string]error{"config validation": loadErr, "the registry": regErr} {
				if err == nil {
					t.Errorf("%s accepted it", name)
					continue
				}
				for _, want := range tt.wants {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s = %q, want it to mention %q", name, err, want)
					}
				}
			}
		})
	}
}

func TestClaudeSbxAnswersThroughTheWholePath(t *testing.T) {
	stream, err := os.ReadFile(filepath.Join("testdata", "claudesbx", "structured_answer.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rig := claudesbxtest.Install(t, string(stream))
	path := claudeSbxConfig(t, claudeSbxTiers)

	repo, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	registry, release, err := modelRegistry(t.Context(), &config.Config{Repo: repo})
	if err != nil {
		t.Fatalf("modelRegistry() = %v, want a registry with no credential set and sbx answering", err)
	}
	defer release()
	definition := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"topics":{"type":"array","items":{"type":"string"}}},"required":["summary","topics"],"additionalProperties":false}`)
	for _, tier := range llm.CompletionTiers {
		t.Run(string(tier), func(t *testing.T) {
			completer, err := registry.Completer(tier)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := completer.Complete(t.Context(), llm.Request{
				System:   "You distill team chat.",
				Messages: []llm.Message{{Role: llm.RoleUser, Text: "Why Postgres?"}},
				Schema:   &llm.Schema{Name: "summary", Definition: definition},
			})
			if err != nil {
				t.Fatalf("Complete() = %v", err)
			}
			var got struct {
				Summary string   `json:"summary"`
				Topics  []string `json:"topics"`
			}
			if err := json.Unmarshal(resp.JSON, &got); err != nil || got.Summary != "Postgres was chosen" || len(got.Topics) != 1 {
				t.Errorf("answer = %s (%v), want the recorded structured output", resp.JSON, err)
			}
		})
	}
	if rig.Runs(t) != 2 {
		t.Errorf("claude ran %d times, want one call per tier", rig.Runs(t))
	}
	if model, _ := rig.Arg(t, "--model"); model != "claude-sonnet-5-5" {
		t.Errorf("the last call used --model %q, want the assert tier's model", model)
	}
	if got, _ := rig.Env(t, "CLAUDE_CODE_MAX_OUTPUT_TOKENS"); got != "4096" {
		t.Errorf("the assert tier's output budget = %q, want the inherited 4096", got)
	}
	var seen int
	for _, u := range registry.Usage() {
		if u.Provider != llm.ProviderClaudeSbx || !u.CostReported || u.CostUSD != 0.5 || u.Calls != 1 {
			t.Errorf("usage = %+v, want one call and the reported cost of 0.5 per tier", u)
		}
		seen++
	}
	if seen != 2 {
		t.Errorf("Usage() has %d rows, want one per tier", seen)
	}
}
