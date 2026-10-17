package llm

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// Optional judgment bounds are intentionally much shorter than baseline calls.
const (
	DefaultJudgmentTimeout      = 2 * time.Second
	DefaultJudgmentTotalTimeout = 5 * time.Second
	// DefaultAssertionJobBudget bounds ALL optional work while an assertion
	// job holds its scope key, not each assertion or integration. Workers create
	// one context with this deadline and pass it to every judgment call.
	DefaultAssertionJobBudget = 5 * time.Second
	MaxAssertionJobBudget     = 30 * time.Second
	DefaultJudgmentAPIKeyEnv  = "HEARSAY_JEV_API_KEY"
	// Defaults from the September 28, 2026 model-limit snapshot. Adapters use
	// a conservative UTF-8 byte upper bound rather than an unverified tokenizer.
	DefaultJudgmentMaxTotalTokens         = 64000
	DefaultJudgmentMaxStateQuestionTokens = 32000
)

// JudgmentTask scopes an acceptance threshold to exactly one question variant.
type JudgmentTask string

// Acceptance for one task must never qualify a different question variant.
const (
	TaskStanceComparison      JudgmentTask = "stance_comparison"
	TaskTopicMatching         JudgmentTask = "topic_matching"
	TaskTopicShortlist        JudgmentTask = "topic_shortlist"
	TaskOutcomeClassification JudgmentTask = "outcome_classification"
)

// AcceptancePolicy is emitted by deliberate evaluation. No policy is shipped.
// Confidence constrains adoption, not authorization or demonstrated correctness.
type AcceptancePolicy struct {
	Name         string
	ModelVersion string
	Threshold    float64
}

// Accepts binds a threshold to the exact resolved version, never to an alias.
func (p AcceptancePolicy) Accepts(modelVersion string, confidence float64) bool {
	return p.Name != "" && p.ModelVersion != "" && modelVersion == p.ModelVersion && probability(confidence) && confidence >= p.Threshold
}

// JudgmentConfig has the ONLY boost switch. Policies affect acceptance, never
// whether any one integration runs. The zero value is disabled and inert.
type JudgmentConfig struct {
	Enabled      bool
	Model        string
	BaseURL      string
	APIKeyEnv    string
	Timeout      time.Duration
	TotalTimeout time.Duration
	// MaxRetries uses the tier convention: zero defaults to one, -1 disables.
	MaxRetries             int
	Backoff                time.Duration
	Cooldown               time.Duration
	CooldownFailures       int
	AssertionJobBudget     time.Duration
	MaxTotalTokens         int
	MaxStateQuestionTokens int
	Policies               map[JudgmentTask]AcceptancePolicy
}

// WithDefaults supplies bounded transport settings, never a model or policy.
func (c JudgmentConfig) WithDefaults() JudgmentConfig {
	if c.APIKeyEnv == "" {
		c.APIKeyEnv = DefaultJudgmentAPIKeyEnv
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultJudgmentTimeout
	}
	if c.TotalTimeout == 0 {
		c.TotalTimeout = DefaultJudgmentTotalTimeout
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 1
	}
	if c.Backoff == 0 {
		c.Backoff = 100 * time.Millisecond
	}
	if c.Cooldown == 0 {
		c.Cooldown = 30 * time.Second
	}
	if c.CooldownFailures == 0 {
		c.CooldownFailures = 3
	}
	if c.AssertionJobBudget == 0 {
		c.AssertionJobBudget = DefaultAssertionJobBudget
	}
	if c.MaxTotalTokens == 0 {
		c.MaxTotalTokens = DefaultJudgmentMaxTotalTokens
	}
	if c.MaxStateQuestionTokens == 0 {
		c.MaxStateQuestionTokens = DefaultJudgmentMaxStateQuestionTokens
	}
	return c
}

// Retries returns the additional attempts after defaults have been applied.
func (c JudgmentConfig) Retries() int {
	if c.MaxRetries < 0 {
		return 0
	}
	return c.MaxRetries
}

// Validate checks bounds even when disabled, without consulting credentials.
func (c JudgmentConfig) Validate() error {
	c = c.WithDefaults()
	if c.Enabled && strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("judgment model must be explicitly configured")
	}
	if c.Timeout <= 0 || c.Timeout > 30*time.Second {
		return fmt.Errorf("judgment timeout must be positive and at most 30s")
	}
	if c.TotalTimeout <= 0 || c.TotalTimeout > 60*time.Second {
		return fmt.Errorf("judgment total_timeout must be positive and at most 1m")
	}
	if c.MaxRetries < -1 || c.MaxRetries > 5 {
		return fmt.Errorf("judgment max_retries must be between -1 and 5")
	}
	if c.Backoff <= 0 || c.Backoff > 5*time.Second {
		return fmt.Errorf("judgment backoff must be positive and at most 5s")
	}
	if c.Cooldown <= 0 || c.Cooldown > 5*time.Minute || c.CooldownFailures < 1 || c.CooldownFailures > 100 {
		return fmt.Errorf("judgment cooldown must be positive and at most 5m; cooldown_failures must be between 1 and 100")
	}
	if c.AssertionJobBudget <= 0 || c.AssertionJobBudget > MaxAssertionJobBudget {
		return fmt.Errorf("judgment assertion_job_budget must be positive and at most 30s")
	}
	if !envVarName(c.APIKeyEnv) {
		return fmt.Errorf("judgment api_key_env must name an environment variable")
	}
	if c.MaxTotalTokens < 1 || c.MaxTotalTokens > DefaultJudgmentMaxTotalTokens || c.MaxStateQuestionTokens < 1 || c.MaxStateQuestionTokens > DefaultJudgmentMaxStateQuestionTokens || c.MaxStateQuestionTokens > c.MaxTotalTokens {
		return fmt.Errorf("judgment token budgets must be positive, state-plus-question at most 32000 and total at most 64000, with state-plus-question no larger than total")
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("judgment base_url must be an absolute http or https URL without credentials, query or fragment")
		}
	}
	for task, p := range c.Policies {
		switch task {
		case TaskStanceComparison, TaskTopicMatching, TaskTopicShortlist, TaskOutcomeClassification:
		default:
			return fmt.Errorf("judgment policy has unknown task")
		}
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.ModelVersion) == "" || p.ModelVersion == "latest" || p.ModelVersion == "auto" {
			return fmt.Errorf("judgment policy requires a name and pinned model_version")
		}
		if math.IsNaN(p.Threshold) || math.IsInf(p.Threshold, 0) || p.Threshold < 0 || p.Threshold > 1 {
			return fmt.Errorf("judgment policy threshold must be between 0 and 1")
		}
	}
	return nil
}
