package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Option is something a registry is built with that is not configuration: the
// two seams a process or a test supplies.
type Option func(*registry)

// WithEnv reads credentials through f rather than from the process's
// environment.
func WithEnv(f func(string) string) Option {
	return func(r *registry) { r.env = f }
}

// WithHTTPClient makes provider adapters use c. The registry applies its own
// per-attempt timeout, so a client with a timeout of its own would be a second
// bound on the same call: leave it unset.
func WithHTTPClient(c *http.Client) Option {
	return func(r *registry) { r.http = c }
}

// registry is the configured tiers, each already built.
type registry struct {
	completers map[Tier]Completer
	embedder   Embedder
	usage      *usageTable

	env  func(string) string
	http *http.Client
}

// NewRegistry builds every tier the configuration names, from the providers it
// is given, and fails if any of them cannot be built. Everything that can be
// known before a call is checked here — an unknown provider, a missing
// credential, a provider that cannot take a system prompt or produce structured
// output — because configuration is read once at startup and a process that
// cannot make a model call should not be serving (ADR-0005, ADR-0009).
//
// A tier the configuration does not name is not built and not defaulted to
// something else: asking for it is [ErrTierNotConfigured].
//
// Every problem is reported, not the first, so that one run of
// `hearsay config validate` or one failed startup lists everything to fix.
func NewRegistry(cfg Config, providers []Provider, opts ...Option) (Registry, error) {
	r := &registry{completers: map[Tier]Completer{}, usage: newUsageTable(), http: &http.Client{}}
	for _, opt := range opts {
		opt(r)
	}

	byName := map[string]Provider{}
	var errs []error
	for _, p := range providers {
		if _, ok := byName[p.Name()]; ok {
			errs = append(errs, fmt.Errorf("two providers are called %q", p.Name()))
			continue
		}
		byName[p.Name()] = p
	}
	for _, tier := range slices.Sorted(maps.Keys(cfg.Tiers)) {
		if !tier.Valid() {
			errs = append(errs, fmt.Errorf("no such model tier %q: want %s, %s or %s", tier, TierDistill, TierAssert, TierEmbed))
		}
	}
	for _, tier := range cfg.Configured() {
		tc := cfg.Tiers[tier].withDefaults(tier)
		if err := tc.Validate(tier); err != nil {
			errs = append(errs, fmt.Errorf("%s tier: %w", tier, err))
			continue
		}
		provider, ok := byName[tc.Provider]
		if !ok {
			errs = append(errs, fmt.Errorf("%s tier: no provider called %q was given to the registry", tier, tc.Provider))
			continue
		}
		if err := r.build(tier, tc, provider); err != nil {
			errs = append(errs, fmt.Errorf("%s tier: %w", tier, err))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return r, nil
}

// build constructs one tier's client and wraps it in the retries, timeout and
// accounting every call gets.
func (r *registry) build(tier Tier, tc TierConfig, p Provider) error {
	caps := p.Capabilities()
	build := Build{Tier: tier, Config: tc, Env: r.env, HTTPClient: r.http}
	client := &tierClient{tier: tier, provider: p.Name(), cfg: tc, usage: r.usage}

	if err := CanServe(p.Name(), caps, tier); err != nil {
		return err
	}

	if !tier.Completion() {
		inner, err := p.Embedder(build)
		if err != nil {
			return err
		}
		r.embedder = &embedder{tierClient: client, inner: inner}
		return nil
	}

	inner, err := p.Completer(build)
	if err != nil {
		return err
	}
	r.completers[tier] = &completer{tierClient: client, inner: inner}
	return nil
}

// CanServe reports whether a provider can back a tier, and says what it is
// missing when it cannot.
//
// Hearsay asks every completion tier for a system prompt and for structured
// output, so a provider without either cannot back one. The answer is the same
// wherever it is asked: [NewRegistry] asks it when a process starts, and
// `hearsay config validate` asks it about a file in a pull request, which is
// where ADR-0005 wants a capability gap to surface — when configuration is
// read, rather than as a degraded call in production.
func CanServe(name string, caps Capabilities, tier Tier) error {
	if !tier.Completion() {
		if !caps.Embed {
			return fmt.Errorf("%s has no embedding model: point the %s tier at a provider that has one", name, tier)
		}
		return nil
	}
	switch {
	case !caps.Complete:
		return fmt.Errorf("%s does not generate text: it can only back the %s tier", name, TierEmbed)
	case !caps.SystemPrompt:
		return fmt.Errorf("%s takes no system prompt, and every prompt in Hearsay has one", name)
	case !caps.StructuredOutput:
		return fmt.Errorf("%s cannot answer with JSON satisfying a schema, which this abstraction promises its callers", name)
	}
	return nil
}

// Completer implements [Registry].
func (r *registry) Completer(t Tier) (Completer, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("no such model tier %q: want %s or %s", t, TierDistill, TierAssert)
	}
	if !t.Completion() {
		return nil, fmt.Errorf("the %s tier has no completer: it is served by an embedder", t)
	}
	c, ok := r.completers[t]
	if !ok {
		return nil, fmt.Errorf("%s: %w", t, ErrTierNotConfigured)
	}
	return c, nil
}

// Embedder implements [Registry].
func (r *registry) Embedder() (Embedder, error) {
	if r.embedder == nil {
		return nil, fmt.Errorf("%s: %w", TierEmbed, ErrTierNotConfigured)
	}
	return r.embedder, nil
}

// Usage implements [Registry].
func (r *registry) Usage() []Usage { return r.usage.snapshot() }

// tierClient is what every tier's client shares: what it is, how it is
// configured, and where its usage is counted.
type tierClient struct {
	tier     Tier
	provider string
	cfg      TierConfig
	usage    *usageTable
}

// completer is one tier's completer, with the abstraction's own concerns around
// the provider's client.
type completer struct {
	*tierClient
	inner Completer
}

// Complete implements [Completer].
func (c *completer) Complete(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, fmt.Errorf("%s tier: %w", c.tier, err)
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = c.cfg.MaxTokens
	}
	resp, err := call(ctx, c.tierClient, func(ctx context.Context) (Response, outcome, error) {
		resp, err := c.inner.Complete(ctx, req)
		return resp, outcome{tokens: resp.Tokens, model: resp.Model}, err
	})
	if err != nil {
		return Response{}, err
	}
	if resp.StopReason == StopRefusal {
		return Response{}, fmt.Errorf("%s tier: %w", c.tier, ErrRefused)
	}
	if req.Schema == nil {
		return resp, nil
	}
	if resp.StopReason == StopMaxTokens {
		return Response{}, fmt.Errorf("%s tier: %w: %d tokens was not enough for %s", c.tier, ErrTruncated, req.MaxTokens, req.Schema.Name)
	}
	if len(resp.JSON) == 0 {
		return Response{}, fmt.Errorf("%s tier: %s answered with no JSON for %s", c.tier, c.provider, req.Schema.Name)
	}
	if err := req.Schema.ValidateJSON(resp.JSON); err != nil {
		return Response{}, fmt.Errorf("%s tier: the answer does not satisfy %s: %w", c.tier, req.Schema.Name, err)
	}
	return resp, nil
}

// embedder is the embed tier's client, wrapped the same way.
type embedder struct {
	*tierClient
	inner Embedder
}

// Dimensions implements [Embedder].
func (e *embedder) Dimensions() int { return e.inner.Dimensions() }

// Embed implements [Embedder]. The width of what comes back is checked here:
// a vector of the wrong length is what would be written into the vector(N)
// column as garbage, and a provider that quietly changed a model's width is
// exactly the case ADR-0005 asks to refuse rather than absorb.
func (e *embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	for i, text := range texts {
		if text == "" {
			return nil, fmt.Errorf("%s tier: texts[%d] is empty: there is no vector for nothing", e.tier, i)
		}
	}
	vectors, err := call(ctx, e.tierClient, func(ctx context.Context) ([][]float32, outcome, error) {
		if reporter, ok := e.inner.(UsageEmbedder); ok {
			vectors, tokens, err := reporter.EmbedTokens(ctx, texts)
			return vectors, outcome{tokens: tokens, model: e.cfg.Model}, err
		}
		vectors, err := e.inner.Embed(ctx, texts)
		return vectors, outcome{model: e.cfg.Model}, err
	})
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("%s tier: %s returned %d vectors for %d texts", e.tier, e.provider, len(vectors), len(texts))
	}
	for i, v := range vectors {
		if len(v) != e.inner.Dimensions() {
			return nil, fmt.Errorf("%s tier: %w: vector %d has %d values and the tier is configured for %d",
				e.tier, ErrDimensions, i, len(v), e.inner.Dimensions())
		}
	}
	return vectors, nil
}

// outcome is what one attempt cost, for the accounting the caller does not do.
type outcome struct {
	tokens Tokens
	model  string
}

// call runs one model call: the per-attempt timeout, the retries with backoff,
// the wait a rate-limited provider asked for, and the token accounting. It is
// the single place that knows how a model call behaves, which is what keeps all
// of it out of the distiller and the assertion worker (ADR-0005).
func call[T any](ctx context.Context, t *tierClient, run func(context.Context) (T, outcome, error)) (T, error) {
	var zero T
	log := telemetry.Logger(ctx)
	attempts := t.cfg.Retries() + 1
	for attempt := 0; ; attempt++ {
		started := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
		v, out, err := run(attemptCtx)
		timedOut := attemptCtx.Err() != nil && ctx.Err() == nil
		cancel()
		took := time.Since(started)

		if err == nil {
			model := out.model
			if model == "" {
				model = t.cfg.Model
			}
			t.usage.answered(usageKey{tier: t.tier, provider: t.provider, model: model}, out.tokens)
			log.LogAttrs(ctx, slog.LevelDebug, "model call",
				slog.String("tier", string(t.tier)), slog.String("provider", t.provider),
				slog.String("model", model), slog.Int("input_tokens", out.tokens.Input),
				slog.Int("output_tokens", out.tokens.Output), slog.Int64("duration_ms", took.Milliseconds()),
				slog.Int("attempt", attempt+1))
			return v, nil
		}

		if timedOut {
			err = &Error{Provider: t.provider, Model: t.cfg.Model, Tier: t.tier, Retryable: true,
				Msg: fmt.Sprintf("the attempt timed out after %s", t.cfg.Timeout), Err: err}
		}
		if ctx.Err() != nil || attempt+1 >= attempts || !Retryable(err) {
			t.usage.failed(usageKey{tier: t.tier, provider: t.provider, model: t.cfg.Model})
			return zero, err
		}

		delay := t.backoff(attempt, err)
		log.LogAttrs(ctx, slog.LevelDebug, "model call failed, retrying",
			slog.String("tier", string(t.tier)), slog.String("provider", t.provider),
			slog.String("model", t.cfg.Model), slog.Int64("duration_ms", took.Milliseconds()),
			slog.Int("attempt", attempt+1), slog.Int64("retry_in_ms", delay.Milliseconds()),
			slog.String("error", err.Error()))
		if err := sleep(ctx, delay); err != nil {
			t.usage.failed(usageKey{tier: t.tier, provider: t.provider, model: t.cfg.Model})
			return zero, err
		}
	}
}

// backoff is how long to wait before the next attempt: what the provider asked
// for if it asked, and otherwise the tier's backoff doubled per attempt and
// capped, with the jitter spread over the whole interval rather than a fraction
// of it, so that a fleet of workers rate-limited at once does not come back in
// step.
func (t *tierClient) backoff(attempt int, err error) time.Duration {
	var e *Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter
	}
	d := t.cfg.Backoff
	for range attempt {
		d *= 2
		if d >= MaxBackoff {
			d = MaxBackoff
			break
		}
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}

// sleep waits, or gives up when the caller's context is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// The registry's own clients satisfy the interfaces callers see.
var (
	_ Registry  = (*registry)(nil)
	_ Completer = (*completer)(nil)
	_ Embedder  = (*embedder)(nil)
)
