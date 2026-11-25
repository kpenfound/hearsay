package config_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/config/configtest"
	"github.com/kpenfound/hearsay/internal/llm"
)

func TestJudgmentsDefaultsAndForms(t *testing.T) {
	for _, tt := range []struct {
		name  string
		files map[string]string
		model string
	}{
		{"absent", base, ""},
		{"directory", with(map[string]string{"llm/judgments.yaml": "judgments:\n  model: pinned-v1\n"}), "pinned-v1"},
		{"single", map[string]string{"hearsay.yaml": "sources:\n  - id: github\n    type: github\n    containers: [acme/api]\nscopes:\n  - id: api\n    sources: [github]\nllm:\n  judgments:\n    model: pinned-v1\n"}, "pinned-v1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := config.Load(configtest.Write(t, tt.files))
			if err != nil {
				t.Fatal(err)
			}
			j := repo.Judgments
			if j.Model != tt.model || j.Threshold != llm.DefaultJudgmentThreshold || j.APIKeyEnv != llm.DefaultJudgmentAPIKeyEnv || j.MaxTotalTokens != 64000 || j.MaxStateQuestionTokens != 32000 {
				t.Fatalf("judgments=%+v", j)
			}
		})
	}
}

func TestJudgmentsThreshold(t *testing.T) {
	repo, err := config.Load(configtest.Write(t, with(map[string]string{"llm/judgments.yaml": "judgments:\n  model: pinned-v1\n  threshold: 0.9\n  api_key_env: OPENROUTER_API_KEY\n"})))
	if err != nil {
		t.Fatal(err)
	}
	if repo.Judgments.Threshold != 0.9 || repo.Judgments.APIKeyEnv != "OPENROUTER_API_KEY" {
		t.Fatalf("judgments=%+v", repo.Judgments)
	}
}

func TestJudgmentsValidation(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"zero threshold", "threshold: 0", "threshold"},
		{"threshold above one", "threshold: 1.1", "threshold"},
		{"nan threshold", "threshold: .nan", "threshold"},
		{"too many retries", "max_retries: 6", "max_retries"},
		{"long call", "total_timeout: 61s", "total_timeout"},
		{"long attempt", "timeout: 31s", "timeout"},
		{"total token limit", "max_total_tokens: 64001", "token budgets"},
		{"state question token limit", "max_state_question_tokens: 32001", "token budgets"},
		{"negative token limit", "max_total_tokens: -1", "token budgets"},
		{"inconsistent token limits", "max_total_tokens: 1000\n  max_state_question_tokens: 2000", "token budgets"},
		{"credential value", "api_key_env: sk-not-a-variable", "api_key_env"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Load(configtest.Write(t, with(map[string]string{"llm/judgments.yaml": "judgments:\n  " + tt.body + "\n"})))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load()=%v want %q", err, tt.want)
			}
		})
	}
}

func TestJudgmentsDuplicateRefused(t *testing.T) {
	_, err := config.Load(configtest.Write(t, with(map[string]string{"llm/a.yaml": "judgments:\n  model: a\n", "llm/b.yaml": "judgments:\n  model: b\n"})))
	if err == nil || !strings.Contains(err.Error(), "already configured") {
		t.Fatalf("Load()=%v", err)
	}
}
