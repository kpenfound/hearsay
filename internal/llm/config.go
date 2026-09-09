package llm

import (
	"fmt"
	"maps"
	"net/url"
	"time"
)

// ProviderAnthropic is the first provider (ADR-0005), and the only one this
// build ships an adapter for. The adapter is internal/llm/anthropic.
const ProviderAnthropic = "anthropic"

// The defaults a tier takes for everything its configuration leaves out. They
// are the shape of the work ADR-0005 describes: one call per artifact, seconds
// rather than minutes, on an API that asks to be backed off rather than
// hammered.
const (
	// DefaultTimeout bounds one attempt, not the call: a retried call may take
	// this long several times over.
	DefaultTimeout = 60 * time.Second
	// DefaultMaxRetries is how many further attempts a transient failure gets.
	DefaultMaxRetries = 3
	// DefaultBackoff is the first retry's delay, doubled per attempt and
	// jittered over the whole interval.
	DefaultBackoff = 500 * time.Millisecond
	// MaxBackoff caps that doubling. A provider that asks for longer with a
	// Retry-After header gets it: the cap is on this package's guess, not on
	// what the provider said.
	MaxBackoff = 30 * time.Second
	// DefaultMaxTokens is the answer budget of a completion tier that does not
	// set one.
	DefaultMaxTokens = 2048
)

// TierConfig is what backs one tier: a provider, a model, and the parameters
// the abstraction applies around them. It is configuration rather than code
// (ADR-0005), so every field here can be set in the `llm:` section of the
// configuration repository (docs/config.md).
//
// A zero value of every field but Provider and Model is a working tier: the
// defaults above are filled in when the registry is built, so only a value
// somebody set can be refused.
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
	// required there, because it has to match the vector(N) column L1 stores
	// them in and the mismatch is checked before anything is written
	// ([CheckDimensions]).
	Dimensions int
	// BaseURL replaces the provider's own endpoint. An operator who wants a
	// gateway in front of a provider points a tier at it here (ADR-0005), and
	// a test points one at a server replaying recorded responses.
	BaseURL string
	// APIKeyEnv is the name of the environment variable holding this
	// provider's credential, where it is not the provider's usual one.
	// Credentials are never in the configuration repository, which is checked
	// into git (ADR-0005, ADR-0009): this is the name of a variable, never a
	// value.
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

// Config is every configured tier. A tier that is absent is not configured, and
// asking for it is [ErrTierNotConfigured] rather than a call to whatever was
// nearest.
type Config struct {
	Tiers map[Tier]TierConfig
}

// Default is the configuration a Hearsay with no `llm:` section runs on: the
// two completion tiers on the first provider, with the model each tier's
// character asks for (ADR-0005 deliberately names no model, so this is the one
// line that gets bumped when a better one ships).
//
// There is no default embed tier. Anthropic has no embedding model and it is
// the only adapter this build ships, so an embed tier is something an operator
// configures rather than something that can be defaulted; until they do,
// [Registry.Embedder] is [ErrTierNotConfigured] and nothing writes a vector.
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
// `dimensions` on
// the distill tier, or a `max_tokens` on the embed tier, is somebody's
// misunderstanding of what they configured, and accepting it in silence is how
// it survives.
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

// withDefaults fills in everything the configuration left out.
func (tc TierConfig) withDefaults(t Tier) TierConfig {
	if tc.MaxTokens == 0 && t.Completion() {
		tc.MaxTokens = DefaultMaxTokens
	}
	if tc.Timeout == 0 {
		tc.Timeout = DefaultTimeout
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
// none: the zero value has to mean "the default" so that a tier nobody
// configured retries, which leaves no other way to ask for no retries at all.
func (tc TierConfig) Retries() int {
	if tc.MaxRetries < 0 {
		return 0
	}
	return tc.MaxRetries
}

// envVarName reports whether s is shaped like an environment variable name: a
// name starting with a letter, in the shape every shell agrees on. It is the
// same rule the `secrets:` of a source is held to (internal/config), for the
// same reason — a credential pasted into a configuration repository that is
// checked into git has to be a loud error rather than a secret in a commit.
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
