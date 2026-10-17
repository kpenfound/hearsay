package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/llm"
)

func TestBoostDefaultsAndForms(t *testing.T) {
	for _, tt := range []struct {
		name    string
		files   map[string]string
		enabled bool
	}{
		{"absent", base, false},
		{"off", with(map[string]string{"llm/boost.yaml": "boost:\n  enabled: false\n"}), false},
		{"directory", with(map[string]string{"llm/boost.yaml": "boost:\n  enabled: true\n  model: pinned-v1\n"}), true},
		{"single", map[string]string{"hearsay.yaml": "sources:\n  - id: github\n    type: github\n    containers: [acme/api]\nscopes:\n  - id: api\n    sources: [github]\nllm:\n  boost:\n    enabled: true\n    model: pinned-v1\n"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := config.Load(writeFiles(t, tt.files))
			if err != nil {
				t.Fatal(err)
			}
			if repo.Boost.Enabled != tt.enabled || repo.Boost.AssertionJobBudget != 5*time.Second || repo.Boost.APIKeyEnv != llm.DefaultJudgmentAPIKeyEnv || len(repo.Boost.Policies) != 0 || repo.Boost.MaxTotalTokens != 64000 || repo.Boost.MaxStateQuestionTokens != 32000 {
				t.Fatalf("boost=%+v", repo.Boost)
			}
		})
	}
}

func TestBoostValidation(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"no model", "enabled: true", "explicitly configured"},
		{"per stance switch", "stance_comparison:\n    enabled: true", "no such field"},
		{"per topic switch", "topic_matching: false", "no such field"},
		{"per outcome switch", "outcome_classification: true", "no such field"},
		{"policy switch", "policies:\n    stance_comparison:\n      enabled: false", "no such field"},
		{"loose bool", "enabled: yes", "true or false"},
		{"budget too long", "assertion_job_budget: 31s", "at most 30s"},
		{"negative budget", "assertion_job_budget: -1s", "negative"},
		{"too many retries", "max_retries: 6", "max_retries"},
		{"long call", "total_timeout: 61s", "total_timeout"},
		{"long attempt", "timeout: 31s", "timeout"},
		{"total token limit", "max_total_tokens: 64001", "token budgets"},
		{"state question token limit", "max_state_question_tokens: 32001", "token budgets"},
		{"negative token limit", "max_total_tokens: -1", "token budgets"},
		{"inconsistent token limits", "max_total_tokens: 1000\n  max_state_question_tokens: 2000", "token budgets"},
		{"credential value", "api_key_env: sk-not-a-variable", "api_key_env"},
		{"unknown task", "policies:\n    all:\n      name: reviewed\n      model_version: pinned-v1\n      threshold: 0.9", "unknown task"},
		{"unpinned", "policies:\n    stance_comparison:\n      name: reviewed\n      threshold: 0.9", "pinned model_version"},
		{"missing threshold", "policies:\n    stance_comparison:\n      name: reviewed\n      model_version: pinned-v1", "explicit threshold"},
		{"bad threshold", "policies:\n    stance_comparison:\n      name: reviewed\n      model_version: pinned-v1\n      threshold: 1.1", "threshold"},
		{"nan threshold", "policies:\n    stance_comparison:\n      name: reviewed\n      model_version: pinned-v1\n      threshold: .nan", "threshold"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(writeFiles(t, with(map[string]string{"llm/boost.yaml": "boost:\n  " + tt.body + "\n"})))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load()=%v want %q", err, tt.want)
			}
		})
	}
}

func TestBoostAcceptancePolicyIsTaskAndVersionScoped(t *testing.T) {
	repo, err := config.Load(writeFiles(t, with(map[string]string{"llm/boost.yaml": "boost:\n  enabled: true\n  model: pinned-v1\n  assertion_job_budget: 3s\n  policies:\n    stance_comparison:\n      name: reviewed\n      model_version: pinned-v1\n      threshold: 0.9\n"})))
	if err != nil {
		t.Fatal(err)
	}
	p := repo.Boost.Policies[llm.TaskStanceComparison]
	if !p.Accepts("pinned-v1", 0.9) || p.Accepts("pinned-v2", 1) || p.Accepts("pinned-v1", 0.89) || len(repo.Boost.Policies) != 1 || repo.Boost.AssertionJobBudget != 3*time.Second {
		t.Fatalf("boost=%+v", repo.Boost)
	}
}

func TestBoostDuplicateRefused(t *testing.T) {
	_, err := config.Load(writeFiles(t, with(map[string]string{"llm/a.yaml": "boost:\n  enabled: false\n", "llm/b.yaml": "boost:\n  enabled: false\n"})))
	if err == nil || !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("Load()=%v", err)
	}
}

func TestJevCannotBeUsedAsTierProvider(t *testing.T) {
	for _, tier := range []string{"distill", "assert", "embed"} {
		t.Run(tier, func(t *testing.T) {
			extra := ""
			if tier == "embed" {
				extra = "    dimensions: 1024\n"
			}
			_, err := config.Load(writeFiles(t, with(map[string]string{"llm/tiers.yaml": "tiers:\n  " + tier + ":\n    provider: jev\n    model: pinned-v1\n" + extra})))
			if err == nil || !strings.Contains(err.Error(), "no such provider") {
				t.Fatalf("Load()=%v", err)
			}
		})
	}
}
