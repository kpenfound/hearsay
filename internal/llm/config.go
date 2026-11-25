package llm

import (
	"fmt"
	"maps"
	"net/url"
	"time"
)

// ProviderAnthropic is the shipped default provider. The adapter is
// internal/llm/anthropic.
const ProviderAnthropic = "anthropic"

// ProviderClaudeSbx is the Claude Code CLI run inside a Docker Sandbox, on the
// host. The adapter is internal/llm/claudesbx.
const ProviderClaudeSbx = "claude-sbx"

// The defaults a tier takes for everything its configuration leaves out.
const (
	// DefaultTimeout bounds one attempt, not the call: a retried call may take
	// this long several times over. A provider whose attempt takes longer by
	// nature says so in [Capabilities.DefaultTimeout].
	DefaultTimeout = 60 * time.Second
	// DefaultMaxRetries is how many further attempts a transient failure gets.
	DefaultMaxRetries = 3
	// DefaultBackoff is the first retry's delay, doubled per attempt and
	// jittered over the whole interval.
	DefaultBackoff = 500 * time.Millisecond
	// MaxBackoff caps that doubling. A Retry-After from the provider is
	// honoured beyond it.
	MaxBackoff = 30 * time.Second
	// DefaultMaxTokens is the answer budget of a completion tier that does not
	// set one.
	DefaultMaxTokens = 2048
)

// TierConfig is what backs one tier: a provider, a model, and the parameters
// the abstraction applies around them, set in the `llm:` section of the
// configuration repository. A zero value of every field but Provider and Model
// is a working tier: the defaults above are filled in when the registry is
// built, so only a value somebody set can be refused.
type TierConfig struct {
	// Provider names the adapter that answers this tier, such as `anthropic`.
	Provider string
	// Model is the provider's model id, passed through untouched.
	Model string
	// MaxTokens is the answer budget, on a completion tier. A [Request] may
	// ask for a different one.
	MaxTokens int
	// Temperature is the provider's, where it has one, on a completion tier.
	// Nil leaves it to the provider.
	Temperature *float64
	// Dimensions is the width of the vectors an embed tier produces. It is
	// required there and checked against the L1 column ([CheckDimensions]).
	Dimensions int
	// BaseURL replaces the provider's own endpoint.
	BaseURL string
	// APIKeyEnv is the name of the environment variable holding this
	// provider's credential, where it is not the provider's usual one.
	// It is the name of a variable, never a credential.
	APIKeyEnv string
	// Timeout bounds one attempt.
	Timeout time.Duration
	// MaxRetries is how many further attempts a transient failure gets. Zero
	// takes the default; a tier that wants none says -1, which is what
	// [TierConfig.Retries] turns back into 0.
	MaxRetries int
	// Backoff is the first retry's delay.
	Backoff time.Duration
}

// Config is every configured tier. Asking for a tier that is absent is
// [ErrTierNotConfigured].
type Config struct {
	Tiers map[Tier]TierConfig
}

// Default is the configuration a Hearsay with no `llm:` section runs on: the
// two completion tiers on the first provider. There is no default embed tier:
// until an operator configures one, [Registry.Embedder] is
// [ErrTierNotConfigured] and nothing writes a vector.
func Default() Config {
	return Config{Tiers: map[Tier]TierConfig{
		TierDistill: {Provider: ProviderAnthropic, Model: "claude-haiku-4-5-20251001", MaxTokens: DefaultMaxTokens},
		TierAssert:  {Provider: ProviderAnthropic, Model: "claude-sonnet-5", MaxTokens: 4096},
	}}
}

// Tier returns the configuration of one tier.
func (c Config) Tier(t Tier) (TierConfig, bool) {
	tc, ok := c.Tiers[t]
	return tc, ok
}

// Configured is every tier this configuration names, in [Tiers] order.
func (c Config) Configured() []Tier {
	out := make([]Tier, 0, len(c.Tiers))
	for _, t := range Tiers {
		if _, ok := c.Tiers[t]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Clone copies the configuration, so that a caller merging its own values over
// a [Default] does not write into a shared map.
func (c Config) Clone() Config {
	return Config{Tiers: maps.Clone(c.Tiers)}
}

// Validate reports what is wrong with one tier's configuration, apart from
// whether the provider exists: that is [NewRegistry]'s to answer, against the
// providers it was given.
//
// A parameter that a tier would not use is refused rather than ignored: a
// `dimensions` on the distill tier, or a `max_tokens` on the embed tier.
func (tc TierConfig) Validate(t Tier) error {
	if !t.Valid() {
		return fmt.Errorf("no such model tier %q", t)
	}
	if tc.Provider == "" {
		return fmt.Errorf("provider is required")
	}
	if tc.Model == "" {
		return fmt.Errorf("model is required")
	}
	if tc.MaxTokens < 0 {
		return fmt.Errorf("max_tokens is %d: it cannot be negative", tc.MaxTokens)
	}
	if tc.Timeout < 0 {
		return fmt.Errorf("timeout is %s: it cannot be negative", tc.Timeout)
	}
	if tc.Backoff < 0 {
		return fmt.Errorf("backoff is %s: it cannot be negative", tc.Backoff)
	}
	if tc.MaxRetries < -1 {
		return fmt.Errorf("max_retries is %d: it cannot be below -1, which is none at all", tc.MaxRetries)
	}
	if tc.Temperature != nil && (*tc.Temperature < 0 || *tc.Temperature > 1) {
		return fmt.Errorf("temperature is %v: it is between 0 and 1", *tc.Temperature)
	}
	if tc.BaseURL != "" {
		u, err := url.Parse(tc.BaseURL)
		if err != nil {
			return fmt.Errorf("base_url %q is not a URL: %w", tc.BaseURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("base_url %q is not an absolute http or https URL", tc.BaseURL)
		}
	}
	if tc.APIKeyEnv != "" && !envVarName(tc.APIKeyEnv) {
		return fmt.Errorf("api_key_env %q is not the name of an environment variable: it names the variable holding the credential, and never the credential itself", tc.APIKeyEnv)
	}
	if t == TierEmbed {
		if tc.Dimensions <= 0 {
			return fmt.Errorf("dimensions is required on the embed tier: it has to match the vector(N) column the embeddings are written to")
		}
		if tc.MaxTokens != 0 {
			return fmt.Errorf("max_tokens is set on the embed tier, which has no answer to budget")
		}
		if tc.Temperature != nil {
			return fmt.Errorf("temperature is set on the embed tier, which does not generate")
		}
		return nil
	}
	if tc.Dimensions != 0 {
		return fmt.Errorf("dimensions is set on the %s tier, which produces text rather than vectors", t)
	}
	return nil
}

func (tc TierConfig) withDefaults(t Tier, providerTimeout time.Duration) TierConfig {
	if tc.MaxTokens == 0 && t.Completion() {
		tc.MaxTokens = DefaultMaxTokens
	}
	if tc.Timeout == 0 {
		tc.Timeout = DefaultTimeout
		if providerTimeout > 0 {
			tc.Timeout = providerTimeout
		}
	}
	if tc.MaxRetries == 0 {
		tc.MaxRetries = DefaultMaxRetries
	}
	if tc.Backoff == 0 {
		tc.Backoff = DefaultBackoff
	}
	return tc
}

// Retries is how many further attempts this tier's calls get, with -1 read as
// none.
func (tc TierConfig) Retries() int {
	if tc.MaxRetries < 0 {
		return 0
	}
	return tc.MaxRetries
}

func envVarName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		switch c := s[i]; {
		case c >= 'A' && c <= 'Z':
		case (c >= '0' && c <= '9') || c == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
