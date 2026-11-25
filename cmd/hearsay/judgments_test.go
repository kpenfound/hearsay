package main

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/llm"
)

func TestJudgmentCapabilityRequiresAModelAndKeyOnceAConfigurationIsLoaded(t *testing.T) {
	provider := func(llm.JudgmentConfig, string) llm.JudgmentProvider { return llm.NewFakeJudgmentProvider() }
	for _, tt := range []struct {
		name, path, model, key, want string
	}{
		{name: "no configuration has nothing to judge", key: ""},
		{name: "a configuration with no model", path: "/etc/hearsay", key: "secret", want: "llm.judgments.model"},
		{name: "a configuration with no key", path: "/etc/hearsay", model: "recorded-v1", want: llm.DefaultJudgmentAPIKeyEnv},
		{name: "a configuration with both", path: "/etc/hearsay", model: "recorded-v1", key: "secret"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(llm.DefaultJudgmentAPIKeyEnv, tt.key)
			cfg := config.Default()
			cfg.Repo.Path = tt.path
			cfg.Repo.Judgments = llm.JudgmentConfig{Model: tt.model}
			j, err := judgmentCapability(&cfg, provider)
			switch {
			case tt.want == "" && (err != nil || j == nil):
				t.Fatalf("judgmentCapability() = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("judgmentCapability() = %v, want an error naming %s", err, tt.want)
			}
		})
	}
}
