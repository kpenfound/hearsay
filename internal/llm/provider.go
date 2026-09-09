package llm

import (
	"fmt"
	"net/http"
	"os"
)

// Provider is one vendor's adapter: the only code in Hearsay that knows what a
// provider's API looks like. Adapters live in a subpackage each
// (internal/llm/anthropic), and adding a vendor is writing one and no other
// change (ADR-0005).
//
// An adapter is thin on purpose. Retries, backoff, rate-limit waits, timeouts
// and token accounting are applied by the registry around whatever it returns,
// so an adapter makes one call, reports what it cost, and says whether trying
// again could work by returning an [*Error].
type Provider interface {
	// Name is what configuration calls this provider.
	Name() string
	// Capabilities is what it can do. A provider that cannot do what a tier
	// needs is refused when the registry is built, which is the whole point of
	// declaring them (ADR-0005).
	Capabilities() Capabilities
	// Completer builds the client for a completion tier.
	Completer(Build) (Completer, error)
	// Embedder builds the client for the embed tier.
	Embedder(Build) (Embedder, error)
}

// Capabilities is what a provider can do. Hearsay asks every completion tier
// for a system prompt and for structured output, so a provider without either
// cannot back one: it is refused when configuration is read rather than
// degrading a call in production (ADR-0005).
type Capabilities struct {
	// Complete is whether the provider generates text at all.
	Complete bool
	// Embed is whether it has an embedding model. Anthropic does not, which is
	// why the embed tier points somewhere else from the first day.
	Embed bool
	// SystemPrompt is whether it takes a system prompt as its own thing.
	SystemPrompt bool
	// StructuredOutput is whether it can be made to answer with JSON
	// satisfying a schema, by any means the adapter chooses.
	StructuredOutput bool
}

// Build is what an adapter is given to construct one tier's client: the tier,
// its configuration, and the two things a provider adapter needs from its
// process — the environment its credential is in, and an HTTP client.
type Build struct {
	// Tier is what this client is for.
	Tier Tier
	// Config is the tier's configuration, with the defaults already filled in.
	Config TierConfig
	// Env reads an environment variable. It is a function rather than
	// [os.Getenv] so that a test can build a registry without writing to the
	// process's environment.
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
// from the configuration repository, which is checked into git (ADR-0005).
//
// A missing credential is an error here, when the registry is built, rather
// than a 401 on the first artifact somebody ingests.
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

// HTTP is the client to make requests with. The registry always sets one; the
// fallback is for an adapter built by hand in a test, and has no timeout of its
// own because the timeout is the registry's.
func (b Build) HTTP() *http.Client {
	if b.HTTPClient == nil {
		return &http.Client{}
	}
	return b.HTTPClient
}
