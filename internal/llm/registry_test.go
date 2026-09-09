package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
)

// cfg is a configuration naming one tier, with retries turned down so that a
// test that exercises them does not sleep for seconds.
func cfg(tier llm.Tier, tc llm.TierConfig) llm.Config {
	if tc.Backoff == 0 {
		tc.Backoff = time.Millisecond
	}
	return llm.Config{Tiers: map[llm.Tier]llm.TierConfig{tier: tc}}
}

func TestNewRegistryRefusesWhatItCannotBuild(t *testing.T) {
	full := llm.Capabilities{Complete: true, Embed: true, SystemPrompt: true, StructuredOutput: true}
	for _, tt := range []struct {
		name     string
		config   llm.Config
		provider *stubProvider
		wantErr  string
	}{
		{
			name:     "a provider nothing supplies",
			config:   cfg(llm.TierDistill, llm.TierConfig{Provider: "openai", Model: "gpt"}),
			provider: newStub("anthropic"),
			wantErr:  `no provider called "openai"`,
		},
		{
			name:     "no provider at all",
			config:   cfg(llm.TierDistill, llm.TierConfig{Model: "m"}),
			provider: newStub("anthropic"),
			wantErr:  "provider is required",
		},
		{
			name:     "no model",
			config:   cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic"}),
			provider: newStub("anthropic"),
			wantErr:  "model is required",
		},
		{
			name:   "a provider with no system prompt",
			config: cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m"}),
			provider: &stubProvider{name: "anthropic",
				caps: llm.Capabilities{Complete: true, StructuredOutput: true}},
			wantErr: "takes no system prompt",
		},
		{
			name:   "a provider with no structured output",
			config: cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m"}),
			provider: &stubProvider{name: "anthropic",
				caps: llm.Capabilities{Complete: true, SystemPrompt: true}},
			wantErr: "cannot answer with JSON",
		},
		{
			name:     "a provider with no embedding model on the embed tier",
			config:   cfg(llm.TierEmbed, llm.TierConfig{Provider: "anthropic", Model: "m", Dimensions: 4}),
			provider: &stubProvider{name: "anthropic", caps: llm.Capabilities{Complete: true, SystemPrompt: true, StructuredOutput: true}},
			wantErr:  "has no embedding model",
		},
		{
			name:     "an embedding-only provider on a completion tier",
			config:   cfg(llm.TierDistill, llm.TierConfig{Provider: "voyage", Model: "m"}),
			provider: &stubProvider{name: "voyage", caps: llm.Capabilities{Embed: true}},
			wantErr:  "does not generate text",
		},
		{
			name:     "an embed tier with no width",
			config:   cfg(llm.TierEmbed, llm.TierConfig{Provider: "anthropic", Model: "m"}),
			provider: newStub("anthropic"),
			wantErr:  "dimensions is required",
		},
		{
			name:     "a width on a tier that produces text",
			config:   cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m", Dimensions: 1024}),
			provider: newStub("anthropic"),
			wantErr:  "dimensions is set on the distill tier",
		},
		{
			name:     "a budget on the tier with no answer to budget",
			config:   cfg(llm.TierEmbed, llm.TierConfig{Provider: "anthropic", Model: "m", Dimensions: 8, MaxTokens: 100}),
			provider: newStub("anthropic"),
			wantErr:  "max_tokens is set on the embed tier",
		},
		{
			name:     "a credential the environment does not hold",
			config:   cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m"}),
			provider: &stubProvider{name: "anthropic", caps: full, failCompleter: errors.New("ANTHROPIC_API_KEY is unset or empty")},
			wantErr:  "ANTHROPIC_API_KEY is unset",
		},
		{
			name:     "a tier that is not one of the three",
			config:   llm.Config{Tiers: map[llm.Tier]llm.TierConfig{"summarize": {Provider: "anthropic", Model: "m"}}},
			provider: newStub("anthropic"),
			wantErr:  "no such model tier",
		},
		{
			name:     "a base_url that is not a URL",
			config:   cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m", BaseURL: "api.anthropic.com"}),
			provider: newStub("anthropic"),
			wantErr:  "absolute http or https URL",
		},
		{
			name:     "a credential pasted in where its name belongs",
			config:   cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m", APIKeyEnv: "sk-ant-not-a-variable"}),
			provider: newStub("anthropic"),
			wantErr:  "name of an environment variable",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := llm.NewRegistry(tt.config, []llm.Provider{tt.provider})
			if err == nil {
				t.Fatalf("NewRegistry() built a registry, want an error about %q", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("NewRegistry() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Every problem is reported, not the first: one failed startup should list
// everything to fix.
func TestNewRegistryReportsEveryProblem(t *testing.T) {
	config := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "nowhere", Model: "m"},
		llm.TierEmbed:   {Provider: "anthropic", Model: "m"},
	}}
	_, err := llm.NewRegistry(config, []llm.Provider{newStub("anthropic")})
	if err == nil {
		t.Fatal("NewRegistry() built a registry, want two problems")
	}
	for _, want := range []string{`no provider called "nowhere"`, "dimensions is required"} {
		if !contains(err.Error(), want) {
			t.Errorf("NewRegistry() = %q, want it to mention %q", err, want)
		}
	}
}

func TestRegistryResolvesTiers(t *testing.T) {
	config := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "small"},
		llm.TierAssert:  {Provider: "anthropic", Model: "strong"},
	}}
	registry, err := llm.NewRegistry(config, []llm.Provider{newStub("anthropic")})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	for _, tier := range llm.CompletionTiers {
		completer, err := registry.Completer(tier)
		if err != nil {
			t.Fatalf("Completer(%s) = %v", tier, err)
		}
		resp, err := completer.Complete(t.Context(), ask("hello"))
		if err != nil {
			t.Fatalf("Complete() = %v", err)
		}
		if want := config.Tiers[tier].Model; resp.Model != want {
			t.Errorf("the %s tier answered as %q, want %q", tier, resp.Model, want)
		}
	}

	// The embed tier is served by an embedder, and asking the wrong way says so
	// rather than falling back to something.
	if _, err := registry.Completer(llm.TierEmbed); err == nil {
		t.Error("Completer(embed) returned a completer")
	}
	// A tier nothing configured is not defaulted to another one.
	if _, err := registry.Embedder(); !errors.Is(err, llm.ErrTierNotConfigured) {
		t.Errorf("Embedder() = %v, want ErrTierNotConfigured", err)
	}
}

// The defaults are filled in where configuration is silent, and only there.
func TestRegistryFillsDefaultsIn(t *testing.T) {
	var builds []llm.Build
	provider := newStub("anthropic")
	provider.builds = &builds
	config := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "small"},
		llm.TierAssert:  {Provider: "anthropic", Model: "strong", MaxTokens: 8192, Timeout: 5 * time.Second, MaxRetries: -1},
	}}
	if _, err := llm.NewRegistry(config, []llm.Provider{provider}); err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	if len(builds) != 2 {
		t.Fatalf("the registry built %d clients, want 2", len(builds))
	}
	byTier := map[llm.Tier]llm.TierConfig{}
	for _, b := range builds {
		byTier[b.Tier] = b.Config
	}
	if got := byTier[llm.TierDistill]; got.MaxTokens != llm.DefaultMaxTokens || got.Timeout != llm.DefaultTimeout ||
		got.MaxRetries != llm.DefaultMaxRetries || got.Backoff != llm.DefaultBackoff {
		t.Errorf("the distill tier was built with %+v, want the defaults", got)
	}
	if got := byTier[llm.TierAssert]; got.MaxTokens != 8192 || got.Timeout != 5*time.Second || got.Retries() != 0 {
		t.Errorf("the assert tier was built with %+v, want what was configured", got)
	}
}

// Swapping a tier's provider and model is a configuration edit and no code
// change: two registries, two configurations, one caller.
func TestSwappingProviderInConfiguration(t *testing.T) {
	answers := map[string]string{"anthropic": "from anthropic", "elsewhere": "from elsewhere"}
	provider := func(name string) *stubProvider {
		p := newStub(name)
		p.complete = func(context.Context, llm.Request) (llm.Response, error) {
			return llm.Response{Text: answers[name], Model: name + "-model", StopReason: llm.StopEnd}, nil
		}
		return p
	}
	providers := []llm.Provider{provider("anthropic"), provider("elsewhere")}

	for _, name := range []string{"anthropic", "elsewhere"} {
		config := cfg(llm.TierDistill, llm.TierConfig{Provider: name, Model: name + "-model"})
		registry, err := llm.NewRegistry(config, providers)
		if err != nil {
			t.Fatalf("NewRegistry(%s) = %v", name, err)
		}
		completer, err := registry.Completer(llm.TierDistill)
		if err != nil {
			t.Fatalf("Completer(distill) = %v", err)
		}
		resp, err := completer.Complete(t.Context(), ask("distill this"))
		if err != nil {
			t.Fatalf("Complete() = %v", err)
		}
		if resp.Text != answers[name] {
			t.Errorf("with %s configured, Complete() = %q, want %q", name, resp.Text, answers[name])
		}
	}
}

func TestCompleteRefusesRequestsNoProviderWouldTake(t *testing.T) {
	for _, tt := range []struct {
		name    string
		req     llm.Request
		wantErr string
	}{
		{name: "no messages", req: llm.Request{System: "s"}, wantErr: "at least one message"},
		{
			name:    "an empty message",
			req:     llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "  "}}},
			wantErr: "the message is empty",
		},
		{
			name:    "a role that is not one",
			req:     llm.Request{Messages: []llm.Message{{Role: "system", Text: "x"}}},
			wantErr: `no such role "system"`,
		},
		{
			name: "the assistant first",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, Text: "hello"}, {Role: llm.RoleUser, Text: "hi"}}},
			wantErr: "start with user and alternate",
		},
		{
			name: "two from the same side",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleUser, Text: "a"}, {Role: llm.RoleUser, Text: "b"}}},
			wantErr: "alternate",
		},
		{
			name: "ending on the assistant",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleUser, Text: "a"}, {Role: llm.RoleAssistant, Text: "b"}}},
			wantErr: "ends with the user message",
		},
		{
			name:    "a negative budget",
			req:     llm.Request{MaxTokens: -1, Messages: []llm.Message{{Role: llm.RoleUser, Text: "a"}}},
			wantErr: "cannot be negative",
		},
		{
			name: "a schema nothing can enforce",
			req: llm.Request{
				Messages: []llm.Message{{Role: llm.RoleUser, Text: "a"}},
				Schema:   &llm.Schema{Name: "x", Definition: json.RawMessage(`{"type":"object","patternProperties":{}}`)},
			},
			wantErr: "patternProperties",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			provider := newStub("anthropic")
			provider.complete = func(context.Context, llm.Request) (llm.Response, error) {
				called = true
				return llm.Response{Text: "ok"}, nil
			}
			registry, err := llm.NewRegistry(cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m"}), []llm.Provider{provider})
			if err != nil {
				t.Fatalf("NewRegistry() = %v", err)
			}
			completer, _ := registry.Completer(llm.TierDistill)
			_, err = completer.Complete(t.Context(), tt.req)
			if err == nil {
				t.Fatalf("Complete() = nil, want an error about %q", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("Complete() = %q, want it to mention %q", err, tt.wantErr)
			}
			if called {
				t.Error("the provider was called with a request this package should have refused")
			}
		})
	}
}

func TestCompleteStructuredOutput(t *testing.T) {
	want := `{"summary":"they shipped","outcome_kind":"decided"}`
	for _, tt := range []struct {
		name     string
		response llm.Response
		wantErr  string
	}{
		{
			name:     "JSON satisfying the schema",
			response: llm.Response{JSON: json.RawMessage(want), StopReason: llm.StopEnd},
		},
		{
			name:     "JSON that does not satisfy it",
			response: llm.Response{JSON: json.RawMessage(`{"summary":"s","outcome_kind":"maybe"}`), StopReason: llm.StopEnd},
			wantErr:  "does not satisfy distillation",
		},
		{
			name:     "cut off at the budget",
			response: llm.Response{JSON: json.RawMessage(`{"summary":"they sh`), StopReason: llm.StopMaxTokens},
			wantErr:  "cut off at the token budget",
		},
		{
			name:     "no JSON at all",
			response: llm.Response{Text: "here you go", StopReason: llm.StopEnd},
			wantErr:  "answered with no JSON",
		},
		{
			name:     "a refusal",
			response: llm.Response{StopReason: llm.StopRefusal},
			wantErr:  "declined to answer",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider := newStub("anthropic")
			provider.complete = func(context.Context, llm.Request) (llm.Response, error) { return tt.response, nil }
			registry, err := llm.NewRegistry(cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m"}), []llm.Provider{provider})
			if err != nil {
				t.Fatalf("NewRegistry() = %v", err)
			}
			completer, _ := registry.Completer(llm.TierDistill)
			req := ask("distill this")
			req.Schema = schema(distillSchema)
			resp, err := completer.Complete(t.Context(), req)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Complete() = %v, want no error", err)
			case tt.wantErr == "":
				if string(resp.JSON) != want {
					t.Errorf("Complete().JSON = %s, want %s", resp.JSON, want)
				}
			case err == nil:
				t.Fatalf("Complete() = %+v, want an error about %q", resp, tt.wantErr)
			case !contains(err.Error(), tt.wantErr):
				t.Errorf("Complete() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// The token budget the tier is configured with is what a request that does not
// ask for its own gets; a request that asks for its own gets that.
func TestCompleteAppliesTheTokenBudget(t *testing.T) {
	var seen []int
	provider := newStub("anthropic")
	provider.complete = func(_ context.Context, req llm.Request) (llm.Response, error) {
		seen = append(seen, req.MaxTokens)
		return llm.Response{Text: "ok", StopReason: llm.StopEnd}, nil
	}
	registry, err := llm.NewRegistry(cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m", MaxTokens: 1024}), []llm.Provider{provider})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	completer, _ := registry.Completer(llm.TierDistill)
	if _, err := completer.Complete(t.Context(), ask("a")); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	req := ask("a")
	req.MaxTokens = 64
	if _, err := completer.Complete(t.Context(), req); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if want := []int{1024, 64}; len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("the provider was asked for budgets %v, want %v", seen, want)
	}
}

func TestRetries(t *testing.T) {
	transient := &llm.Error{Provider: "anthropic", Status: 529, Kind: "overloaded_error", Msg: "overloaded", Retryable: true}
	permanent := &llm.Error{Provider: "anthropic", Status: 400, Kind: "invalid_request_error", Msg: "no such model"}

	for _, tt := range []struct {
		name         string
		fail         int
		err          error
		maxRetries   int
		wantAttempts int
		wantErr      string
	}{
		{name: "a transient failure is retried", fail: 2, err: transient, wantAttempts: 3},
		{name: "a permanent failure is not", fail: 2, err: permanent, wantAttempts: 1, wantErr: "no such model"},
		{name: "retries run out", fail: 9, err: transient, wantAttempts: 4, wantErr: "overloaded"},
		{name: "a tier may ask for none", fail: 9, err: transient, maxRetries: -1, wantAttempts: 1, wantErr: "overloaded"},
		{name: "or for more", fail: 4, err: transient, maxRetries: 5, wantAttempts: 5},
		{name: "an error this package does not know is not retried", fail: 9, err: errors.New("something else"), wantAttempts: 1, wantErr: "something else"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int64
			provider := newStub("anthropic")
			provider.complete = func(context.Context, llm.Request) (llm.Response, error) {
				if attempts.Add(1) <= int64(tt.fail) {
					return llm.Response{}, tt.err
				}
				return llm.Response{Text: "ok", StopReason: llm.StopEnd}, nil
			}
			registry, err := llm.NewRegistry(cfg(llm.TierDistill, llm.TierConfig{
				Provider: "anthropic", Model: "m", MaxRetries: tt.maxRetries}), []llm.Provider{provider})
			if err != nil {
				t.Fatalf("NewRegistry() = %v", err)
			}
			completer, _ := registry.Completer(llm.TierDistill)
			_, err = completer.Complete(t.Context(), ask("a"))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Complete() = %v, want no error", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Complete() = nil, want an error about %q", tt.wantErr)
			case tt.wantErr != "" && !contains(err.Error(), tt.wantErr):
				t.Errorf("Complete() = %q, want it to mention %q", err, tt.wantErr)
			}
			if got := int(attempts.Load()); got != tt.wantAttempts {
				t.Errorf("the provider was called %d times, want %d", got, tt.wantAttempts)
			}
		})
	}
}

// A rate-limited provider that says how long to wait is waited for, rather than
// being asked again on this package's own schedule.
func TestRetryAfterIsHonoured(t *testing.T) {
	var attempts atomic.Int64
	var gap time.Duration
	started := time.Now()
	provider := newStub("anthropic")
	provider.complete = func(context.Context, llm.Request) (llm.Response, error) {
		if attempts.Add(1) == 1 {
			return llm.Response{}, &llm.Error{Status: 429, Kind: "rate_limit_error", Msg: "slow down",
				Retryable: true, RetryAfter: 120 * time.Millisecond}
		}
		gap = time.Since(started)
		return llm.Response{Text: "ok", StopReason: llm.StopEnd}, nil
	}
	registry, err := llm.NewRegistry(cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "m"}), []llm.Provider{provider})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	completer, _ := registry.Completer(llm.TierDistill)
	if _, err := completer.Complete(t.Context(), ask("a")); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if gap < 120*time.Millisecond {
		t.Errorf("the second attempt came after %s, want at least the 120ms the provider asked for", gap)
	}
}

// The per-attempt timeout is the abstraction's, so an adapter that hangs is
// still bounded — and a timeout is transient, so it is retried.
func TestTimeoutBoundsAnAttempt(t *testing.T) {
	var attempts atomic.Int64
	provider := newStub("anthropic")
	provider.complete = func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		if attempts.Add(1) == 1 {
			<-ctx.Done()
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{Text: "ok", StopReason: llm.StopEnd}, nil
	}
	registry, err := llm.NewRegistry(cfg(llm.TierDistill, llm.TierConfig{
		Provider: "anthropic", Model: "m", Timeout: 20 * time.Millisecond}), []llm.Provider{provider})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	completer, _ := registry.Completer(llm.TierDistill)
	resp, err := completer.Complete(t.Context(), ask("a"))
	if err != nil {
		t.Fatalf("Complete() = %v, want the timed-out attempt to be retried", err)
	}
	if resp.Text != "ok" || attempts.Load() != 2 {
		t.Errorf("Complete() = %+v after %d attempts, want the second attempt's answer", resp, attempts.Load())
	}
}

// The caller's context stops the retry loop: a worker shutting down does not
// sit through a backoff.
func TestCancellationStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var attempts atomic.Int64
	provider := newStub("anthropic")
	provider.complete = func(context.Context, llm.Request) (llm.Response, error) {
		attempts.Add(1)
		cancel()
		return llm.Response{}, &llm.Error{Status: 529, Msg: "overloaded", Retryable: true}
	}
	registry, err := llm.NewRegistry(cfg(llm.TierDistill, llm.TierConfig{
		Provider: "anthropic", Model: "m", Backoff: time.Minute}), []llm.Provider{provider})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	completer, _ := registry.Completer(llm.TierDistill)
	if _, err := completer.Complete(ctx, ask("a")); err == nil {
		t.Fatal("Complete() = nil, want the cancelled call to fail")
	}
	if attempts.Load() != 1 {
		t.Errorf("the provider was called %d times, want 1: a cancelled context is not retried", attempts.Load())
	}
}

func TestEmbedder(t *testing.T) {
	config := cfg(llm.TierEmbed, llm.TierConfig{Provider: "anthropic", Model: "embed-1", Dimensions: 4})
	registry, err := llm.NewRegistry(config, []llm.Provider{newStub("anthropic")})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	embedder, err := registry.Embedder()
	if err != nil {
		t.Fatalf("Embedder() = %v", err)
	}
	if got := embedder.Dimensions(); got != 4 {
		t.Errorf("Dimensions() = %d, want 4", got)
	}
	vectors, err := embedder.Embed(t.Context(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed() = %v", err)
	}
	if len(vectors) != 3 {
		t.Fatalf("Embed() returned %d vectors, want 3", len(vectors))
	}
	for i, v := range vectors {
		if len(v) != 4 {
			t.Errorf("vector %d has %d values, want 4", i, len(v))
		}
		if v[0] != float32(i) {
			t.Errorf("vector %d is the vector for input %v, want the inputs in order", i, v[0])
		}
	}
	// A completion tier is not an embedder.
	if _, err := registry.Completer(llm.TierDistill); !errors.Is(err, llm.ErrTierNotConfigured) {
		t.Errorf("Completer(distill) = %v, want ErrTierNotConfigured", err)
	}
}

// Vectors of the wrong width are what would be written into the vector(N)
// column as garbage, so they are refused rather than returned.
func TestEmbedRefusesTheWrongShape(t *testing.T) {
	for _, tt := range []struct {
		name    string
		embed   func(context.Context, []string) ([][]float32, llm.Tokens, error)
		texts   []string
		wantErr string
	}{
		{
			name: "a vector of the wrong width",
			embed: func(context.Context, []string) ([][]float32, llm.Tokens, error) {
				return [][]float32{{1, 2}}, llm.Tokens{}, nil
			},
			texts:   []string{"a"},
			wantErr: "2 values and the tier is configured for 4",
		},
		{
			name: "fewer vectors than texts",
			embed: func(context.Context, []string) ([][]float32, llm.Tokens, error) {
				return [][]float32{{1, 2, 3, 4}}, llm.Tokens{}, nil
			},
			texts:   []string{"a", "b"},
			wantErr: "1 vectors for 2 texts",
		},
		{
			name:    "an empty text, which has no vector",
			texts:   []string{"a", ""},
			wantErr: "texts[1] is empty",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider := newStub("anthropic")
			provider.embed = tt.embed
			registry, err := llm.NewRegistry(cfg(llm.TierEmbed, llm.TierConfig{
				Provider: "anthropic", Model: "embed-1", Dimensions: 4}), []llm.Provider{provider})
			if err != nil {
				t.Fatalf("NewRegistry() = %v", err)
			}
			embedder, _ := registry.Embedder()
			_, err = embedder.Embed(t.Context(), tt.texts)
			if err == nil {
				t.Fatalf("Embed() = nil, want an error about %q", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("Embed() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Cost is observable per tier by construction: every call goes through one
// place that knows which tier, provider and model it was (ADR-0005).
func TestUsageIsCountedPerTierProviderAndModel(t *testing.T) {
	provider := newStub("anthropic")
	provider.complete = func(_ context.Context, req llm.Request) (llm.Response, error) {
		if strings.Contains(req.Messages[0].Text, "fail") {
			return llm.Response{}, &llm.Error{Status: 400, Msg: "no"}
		}
		return llm.Response{Text: "ok", StopReason: llm.StopEnd, Model: "small-2", Tokens: llm.Tokens{Input: 10, Output: 3}}, nil
	}
	config := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "small", Backoff: time.Millisecond},
		llm.TierEmbed:   {Provider: "anthropic", Model: "embed-1", Dimensions: 4, Backoff: time.Millisecond},
	}}
	registry, err := llm.NewRegistry(config, []llm.Provider{provider})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	completer, _ := registry.Completer(llm.TierDistill)
	for range 2 {
		if _, err := completer.Complete(t.Context(), ask("distill this")); err != nil {
			t.Fatalf("Complete() = %v", err)
		}
	}
	if _, err := completer.Complete(t.Context(), ask("fail this")); err == nil {
		t.Fatal("Complete() = nil, want the failure the case sets up")
	}
	embedder, _ := registry.Embedder()
	if _, err := embedder.Embed(t.Context(), []string{"a", "b"}); err != nil {
		t.Fatalf("Embed() = %v", err)
	}

	want := []llm.Usage{
		// The model that answered is what is counted, not the model that was
		// asked for: a provider may resolve an alias.
		{Tier: llm.TierDistill, Provider: "anthropic", Model: "small", Failures: 1},
		{Tier: llm.TierDistill, Provider: "anthropic", Model: "small-2", Calls: 2, InputTokens: 20, OutputTokens: 6},
		{Tier: llm.TierEmbed, Provider: "anthropic", Model: "embed-1", Calls: 1, InputTokens: 2},
	}
	got := registry.Usage()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Usage() = %+v\nwant %+v", got, want)
	}
}
