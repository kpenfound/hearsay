package llm

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"time"
)

// Jev's bounds. A job whose judgments fail errors and the queue retries it.
const (
	DefaultJudgmentTimeout      = 2 * time.Second
	DefaultJudgmentTotalTimeout = 5 * time.Second
	DefaultJudgmentAPIKeyEnv    = "HEARSAY_JEV_API_KEY"
	// DefaultJudgmentThreshold is the confidence a yes must reach to count.
	// Anything less, and every no, is a no.
	DefaultJudgmentThreshold = 0.8
	// Adapters measure state with a conservative UTF-8 byte upper bound rather
	// than a tokenizer.
	DefaultJudgmentMaxTotalTokens         = 64000
	DefaultJudgmentMaxStateQuestionTokens = 32000
)

// JudgmentTask names one atomic yes/no judgment. Callers combine judgments
// with code into decisions: Jev makes judgments, code makes decisions.
type JudgmentTask string

// The judgments Hearsay asks. Every one is a yes/no question.
const (
	// TaskStanceSameAnswer: does a new position give the same answer to a
	// topic as its current position? Yes is restates, no is changes.
	TaskStanceSameAnswer JudgmentTask = "stance.same_answer"
	// TaskTopicSameQuestion: does an assertion answer the same question as one
	// candidate topic? Asked once per candidate.
	TaskTopicSameQuestion JudgmentTask = "topic.same_question"
	// The outcome judgments, asked together on a document's text. Code maps
	// the first yes, in this order, to resolved, decided, proposed or open, and
	// four noes to none.
	TaskOutcomeWorkFinished     JudgmentTask = "outcome.work_finished"
	TaskOutcomeBindingAgreement JudgmentTask = "outcome.binding_agreement"
	TaskOutcomeUnagreedProposal JudgmentTask = "outcome.unagreed_proposal"
	TaskOutcomeStillOpen        JudgmentTask = "outcome.still_open"
)

// JudgmentTasks is every task Hearsay asks, in a stable order.
func JudgmentTasks() []JudgmentTask {
	return []JudgmentTask{TaskStanceSameAnswer, TaskTopicSameQuestion,
		TaskOutcomeWorkFinished, TaskOutcomeBindingAgreement, TaskOutcomeUnagreedProposal, TaskOutcomeStillOpen}
}

// Known reports whether t is a task this build asks.
func (t JudgmentTask) Known() bool { return slices.Contains(JudgmentTasks(), t) }

// JudgmentConfig configures Jev. The distiller and the assertion worker
// require it: [NewJudgments] refuses a configuration with no model or no key.
type JudgmentConfig struct {
	Model     string
	BaseURL   string
	APIKeyEnv string
	// Threshold is the confidence a yes must reach to count ([JudgmentConfig.Yes]).
	Threshold    float64
	Timeout      time.Duration
	TotalTimeout time.Duration
	// MaxRetries uses the tier convention: zero defaults to one, -1 disables.
	MaxRetries             int
	Backoff                time.Duration
	MaxTotalTokens         int
	MaxStateQuestionTokens int
}

// WithDefaults supplies bounded transport settings and the threshold, never a
// model.
func (c JudgmentConfig) WithDefaults() JudgmentConfig {
	if c.APIKeyEnv == "" {
		c.APIKeyEnv = DefaultJudgmentAPIKeyEnv
	}
	if c.Threshold == 0 {
		c.Threshold = DefaultJudgmentThreshold
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

// Yes reports whether an answer is a yes that counts: a yes whose confidence
// reaches the threshold. Every other answer is a no.
func (c JudgmentConfig) Yes(a JudgmentAnswer) bool {
	return a.Yes != nil && *a.Yes && probability(a.Confidence) && a.Confidence >= c.WithDefaults().Threshold
}

// Validate checks bounds, without consulting credentials. A missing model is
// [NewJudgments]'s refusal, not a configuration error, so a configuration only
// the API reads need not name one.
func (c JudgmentConfig) Validate() error {
	c = c.WithDefaults()
	if math.IsNaN(c.Threshold) || c.Threshold <= 0 || c.Threshold > 1 {
		return fmt.Errorf("judgment threshold must be above 0 and at most 1")
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
	return nil
}
