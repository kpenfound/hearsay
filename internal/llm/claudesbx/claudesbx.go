// Package claudesbx is the claude-sbx adapter: the Claude Code CLI run for one
// tool-free turn inside a Docker Sandbox, behind internal/llm's [llm.Provider].
// It is a completion provider that needs no API key.
//
// Every call is agentcore's [agent.Completer.Complete]: `claude -p` run in a
// sandbox that holds one empty temporary directory of its own and nothing else
// of the host, with no tools, no skills, no setting sources and no MCP server.
// A tier keeps up to [Sandboxes] sandboxes warm while calls keep coming, and
// gives back any idle for [SandboxIdleTimeout]. agentcore gives each call a
// fresh directory and session of its own, discards a sandbox a call broke, and
// [llm.Registry.Close] removes them all. The sandbox keeps ingested text, which
// is untrusted, away from the owner's files, and the sandbox's proxy supplies
// the Claude Code login: Hearsay never holds, reads or passes a model
// credential for this provider.
//
// It is host-only. It needs the `sbx` CLI, its daemon and a stored login
// (`sbx secret set`) on the machine running the process, so it does not work in
// the Compose or Helm deployments, and the registry refuses to be built where
// `sbx` does not answer (see [Provider.Completer]).
//
// Retries, backoff, the per-attempt timeout, schema validation and token
// accounting are the registry's. Costs the CLI reports are passed on as
// reported and no price is derived from tokens.
//
// A tier that sets `temperature`, `base_url` or `api_key_env` is refused: the
// CLI has no temperature, no endpoint to replace and no key to name. That is
// declared in [Provider.Capabilities] and checked by [llm.CanServe].
// `max_tokens` is the output-token budget, delivered to the CLI as
// CLAUDE_CODE_MAX_OUTPUT_TOKENS.
//
// No error this adapter returns carries a prompt, an answer or agentcore's
// result text.
//
// docs/config.md documents the provider for operators.
package claudesbx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kpenfound/agentcore/agent"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// DefaultTimeout is one attempt's timeout when a tier configures none. It
// leaves room for a sandbox start and a long answer, and stays inside the two
// minutes the distiller and the assertion worker bound a baseline call to.
const DefaultTimeout = 105 * time.Second

// Sandboxes is how many sandboxes one tier keeps at most, and so how many of
// its calls run at once. The assertion worker's calls are serialized and use
// one.
const Sandboxes = 4

// SandboxIdleTimeout removes a kept sandbox no call has used for this long.
const SandboxIdleTimeout = 5 * time.Minute

// SandboxMemory bounds each sandbox's VM.
const SandboxMemory = "1536m"

// RegistryRateLimitWait is how long a call refused for Docker Hub's pull rate
// limit asks to be left alone.
const RegistryRateLimitWait = 10 * time.Minute

const namePrefix = "hearsay-"

// Provider is the adapter. It holds nothing: what a call needs comes from the
// [llm.Build] the registry constructs a client with.
type Provider struct{}

// New is the provider to hand to [llm.NewRegistry].
func New() *Provider { return &Provider{} }

// Name implements [llm.Provider].
func (p *Provider) Name() string { return llm.ProviderClaudeSbx }

// Capabilities implements [llm.Provider]. It completes, takes a system prompt
// and answers with schema-checked JSON; it has no embedding model, takes no
// temperature, endpoint or key, and its attempts are slower than the registry's
// default allows for.
func (p *Provider) Capabilities() llm.Capabilities {
	return llm.Capabilities{
		Complete: true, Embed: false, SystemPrompt: true, StructuredOutput: true,
		NoTemperature: true, NoBaseURL: true, NoAPIKeyEnv: true,
		DefaultTimeout: DefaultTimeout,
	}
}

// Completer implements [llm.Provider]. Where this process cannot use Docker
// Sandboxes it fails here, when the registry is built.
func (p *Provider) Completer(b llm.Build) (llm.Completer, error) {
	runner := &agent.Runner{NamePrefix: namePrefix, Logger: quiet()}
	if err := runner.CheckSandbox(context.Background()); err != nil {
		return nil, fmt.Errorf("%s needs Docker Sandboxes on the machine running this process, and sbx is not available (it is host-only, so not in a container): %w",
			llm.ProviderClaudeSbx, err)
	}
	timeout := b.Config.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &client{
		tier:      b.Tier,
		model:     b.Config.Model,
		maxTokens: b.Config.MaxTokens,
		timeout:   timeout,
		sandboxes: runner.NewCompleter(agent.CompleterOptions{Name: string(b.Tier), Size: Sandboxes, SandboxMemory: SandboxMemory, IdleTimeout: SandboxIdleTimeout}),
	}, nil
}

// Embedder implements [llm.Provider]. This provider has no embedding model.
func (p *Provider) Embedder(llm.Build) (llm.Embedder, error) {
	return nil, fmt.Errorf("%s has no embedding model: point the embed tier at a provider that has one", llm.ProviderClaudeSbx)
}

type client struct {
	tier      llm.Tier
	model     string
	maxTokens int
	timeout   time.Duration
	sandboxes *agent.Completer
}

func (c *client) Close(ctx context.Context) error { return c.sandboxes.Close(ctx) }

func (c *client) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	creq, err := c.request(req)
	if err != nil {
		return llm.Response{}, err
	}
	done, err := c.sandboxes.Complete(ctx, creq)
	if err != nil {
		failed, attrs := c.failure(ctx, err)
		telemetry.Logger(ctx).LogAttrs(ctx, slog.LevelDebug, "claude-sbx call failed",
			append([]slog.Attr{
				slog.String("tier", string(c.tier)),
				slog.String("model", c.model),
				slog.String("error_type", fmt.Sprintf("%T", err)),
			}, attrs...)...)
		return llm.Response{}, failed
	}
	return c.response(req, done)
}

func (c *client) request(req llm.Request) (agent.CompletionRequest, error) {
	if strings.TrimSpace(req.System) == "" {
		return agent.CompletionRequest{}, c.err("the request has no system prompt, which this provider replaces the CLI's own with", false, nil)
	}
	if len(req.System) > agent.MaxCompletionSystemPrompt {
		return agent.CompletionRequest{}, c.err(fmt.Sprintf("the system prompt is %d bytes, over the %d this provider takes", len(req.System), agent.MaxCompletionSystemPrompt), false, nil)
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.maxTokens
	}
	out := agent.CompletionRequest{
		Name:            string(c.tier),
		Model:           c.model,
		SystemPrompt:    req.System,
		Prompt:          prompt(req.Messages),
		MaxOutputTokens: maxTokens,
		Timeout:         c.timeout,
	}
	if req.Schema != nil {
		out.Schema = req.Schema.Definition
	}
	return out, nil
}

func prompt(messages []llm.Message) string {
	if len(messages) == 1 {
		return messages[0].Text
	}
	var b strings.Builder
	for i, m := range messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(label(m.Role))
		b.WriteString(":\n")
		b.WriteString(m.Text)
	}
	return b.String()
}

func label(r llm.Role) string {
	switch r {
	case llm.RoleAssistant:
		return "Assistant"
	default:
		return "User"
	}
}

func (c *client) response(req llm.Request, done *agent.Completion) (llm.Response, error) {
	in, out := done.InputTokens, done.OutputTokens
	usage := &llm.JudgmentUsage{InputTokens: &in, OutputTokens: &out}
	if done.CostKnown {
		cost := done.CostUSD
		usage.CostUSD = &cost
	}
	resp := llm.Response{
		Model:         done.Model,
		StopReason:    llm.StopEnd,
		ReportedUsage: usage,
		Tokens:        llm.Tokens{Input: int(done.InputTokens), Output: int(done.OutputTokens)},
	}
	if resp.Model == "" {
		resp.Model = c.model
	}
	if req.Schema != nil {
		if len(done.StructuredOutput) == 0 {
			return resp, c.err("the answer has no structured output", false, agent.ErrNoStructuredOutput)
		}
		resp.JSON = done.StructuredOutput
		return resp, nil
	}
	if strings.TrimSpace(done.Text) == "" {
		return resp, c.err("the answer has no text in it", false, nil)
	}
	resp.Text = done.Text
	return resp, nil
}

func (c *client) failure(ctx context.Context, err error) (*llm.Error, []slog.Attr) {
	var limit *agent.UsageLimitError
	switch {
	case errors.As(err, &limit):

		var wait time.Duration
		kind := "usage_limit"
		if limit.Window != "" {
			kind += "_" + limit.Window
		}
		if !limit.ResetsAt.IsZero() {
			wait = max(time.Until(limit.ResetsAt), 0)
		}
		msg := "the Claude subscription's usage limit was reached"
		if !limit.ResetsAt.IsZero() {
			msg += ", resets " + limit.ResetsAt.UTC().Format(time.RFC3339)
		}
		e := c.err(msg, true, nil)
		e.Kind, e.RetryAfter = kind, wait
		attrs := []slog.Attr{slog.String("class", "usage_limit"), slog.String("window", limit.Window)}
		if !limit.ResetsAt.IsZero() {
			attrs = append(attrs, slog.Time("resets_at", limit.ResetsAt))
		}
		return e, attrs
	case errors.Is(err, agent.ErrRegistryRateLimited):

		e := c.err("Docker Hub's pull rate limit was reached creating the sandbox", true, agent.ErrRegistryRateLimited)
		e.Kind, e.RetryAfter = "registry_rate_limit", RegistryRateLimitWait
		return e, classOnly("registry_rate_limit")
	case errors.Is(err, context.DeadlineExceeded):

		return c.err("the sandboxed call did not finish in time", true, context.DeadlineExceeded), classOnly("deadline")
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return c.err("the sandboxed call was cancelled", false, context.Canceled), classOnly("cancelled")
	case errors.Is(err, agent.ErrNoStructuredOutput):

		e := c.err("the turn ended without structured output (it may have stopped at the output budget or declined)", false, agent.ErrNoStructuredOutput)
		e.Kind = "no_structured_output"
		attrs := classOnly("no_structured_output")
		var none *agent.NoStructuredOutputError
		if errors.As(err, &none) {
			attrs = append(attrs, slog.String("subtype", none.Subtype))
		}
		return e, attrs
	default:

		return c.err("the sandboxed call failed", false, nil), classOnly("other")
	}
}

func classOnly(class string) []slog.Attr { return []slog.Attr{slog.String("class", class)} }

func (c *client) err(msg string, retryable bool, wrapped error) *llm.Error {
	return &llm.Error{
		Provider:  llm.ProviderClaudeSbx,
		Model:     c.model,
		Tier:      c.tier,
		Msg:       msg,
		Retryable: retryable,
		Err:       wrapped,
	}
}

var (
	_ llm.Provider  = (*Provider)(nil)
	_ llm.Completer = (*client)(nil)
	_ llm.Closer    = (*client)(nil)
)

func quiet() *slog.Logger {
	return slog.New(textless{slog.Default().Handler()})
}

type textless struct{ slog.Handler }

var safeAttrs = map[string]bool{
	"session": true, "role": true, "sandbox": true, "resource": true, "dir": true, "cap_usd": true,
}

func (h textless) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.Handler.Enabled(ctx, l)
}

func (h textless) Handle(ctx context.Context, r slog.Record) error {
	kept := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		kept.AddAttrs(textlessAttrs(a)...)
		return true
	})
	return h.Handler.Handle(ctx, kept)
}

func (h textless) WithAttrs(a []slog.Attr) slog.Handler {
	var kept []slog.Attr
	for _, x := range a {
		kept = append(kept, textlessAttrs(x)...)
	}
	return textless{h.Handler.WithAttrs(kept)}
}

func (h textless) WithGroup(name string) slog.Handler { return textless{h.Handler.WithGroup(name)} }

func textlessAttrs(a slog.Attr) []slog.Attr {
	a.Value = a.Value.Resolve()
	switch {
	case a.Key == "err":
		return []slog.Attr{slog.String("err_type", fmt.Sprintf("%T", a.Value.Any()))}
	case safeAttrs[a.Key] && a.Value.Kind() != slog.KindGroup:
		return []slog.Attr{a}
	}
	return nil
}
