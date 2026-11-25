package llm

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// Provider is one vendor's adapter: the only code in Hearsay that knows what a
// provider's API looks like. Retries, backoff, rate-limit waits, timeouts and
// token accounting are applied by the registry around whatever it returns, so
// an adapter makes one call, reports what it cost, and says whether trying
// again could work by returning an [*Error].
type Provider interface {
	// Name is what configuration calls this provider.
	Name() string
	// Capabilities is what it can do. A provider that cannot do what a tier
	// needs is refused when the registry is built.
	Capabilities() Capabilities
	// Completer builds the client for a completion tier.
	Completer(Build) (Completer, error)
	// Embedder builds the client for the embed tier.
	Embedder(Build) (Embedder, error)
}

// Capabilities is what a provider can do. Every completion tier needs a system
// prompt and structured output, so a provider without either cannot back one.
type Capabilities struct {
	// Complete is whether the provider generates text at all.
	Complete bool
	// Embed is whether it has an embedding model.
	Embed bool
	// SystemPrompt is whether it takes a system prompt as its own thing.
	SystemPrompt bool
	// StructuredOutput is whether it can be made to answer with JSON
	// satisfying a schema, by any means the adapter chooses.
	StructuredOutput bool

	// NoTemperature, NoBaseURL and NoAPIKeyEnv are settings the provider has
	// no use for. A tier whose configuration sets one is refused, by
	// [CanServe] and so by `hearsay config validate` and the registry alike.
	// Only what a configuration sets is refused: a default never is.
	NoTemperature bool
	NoBaseURL     bool
	NoAPIKeyEnv   bool

	// DefaultTimeout is the per-attempt timeout a tier of this provider takes
	// when its configuration sets none. Zero takes [DefaultTimeout]. A
	// configured timeout always wins.
	DefaultTimeout time.Duration
}

// Build is what an adapter is given to construct one tier's client: the tier,
// its configuration, the environment its credential is in, and an HTTP client.
type Build struct {
	// Tier is what this client is for.
	Tier Tier
	// Config is the tier's configuration, with the defaults already filled in.
	Config TierConfig
	// Env reads an environment variable. Nil reads the process environment.
	Env func(string) string
	// HTTPClient is the client to make requests with. Timeouts are the
	// registry's, so an adapter must not set one of its own.
	HTTPClient *http.Client
}

// Getenv reads an environment variable through whatever the registry was given.
func (b Build) Getenv(name string) string {
	if b.Env == nil {
		return os.Getenv(name)
	}
	return b.Env(name)
}

// APIKey is the credential for this tier: the variable the configuration names,
// or the provider's usual one. Credentials come from the environment and never
// from the configuration repository. A missing credential is an error when the
// registry is built.
func (b Build) APIKey(defaultEnv string) (string, error) {
	name := b.Config.APIKeyEnv
	if name == "" {
		name = defaultEnv
	}
	key := b.Getenv(name)
	if key == "" {
		return "", fmt.Errorf("%s is unset or empty: the %s tier's credential comes from the environment, never from configuration", name, b.Tier)
	}
	return key, nil
}

// HTTP is the client to make requests with. The fallback has no timeout of its
// own, because the timeout is the registry's.
func (b Build) HTTP() *http.Client {
	if b.HTTPClient == nil {
		return &http.Client{}
	}
	return b.HTTPClient
}
