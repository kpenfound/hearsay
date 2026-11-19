// Package claudesbx is the claude-sbx adapter: the Claude Code CLI run for one
// tool-free turn inside a Docker Sandbox, behind internal/llm's [llm.Provider].
// It is the second completion provider, and the one that needs no API key.
//
// Every call is agentcore's [agent.Completer.Complete]: `claude -p` run in a
// sandbox that holds one empty temporary directory of its own and nothing else
// of the host, with no tools, no skills, no setting sources and no MCP server.
// A tier keeps up to [Sandboxes] sandboxes warm while calls keep coming, and
// gives back any idle for [SandboxIdleTimeout], so `sbx create` runs once per
// sandbox per burst rather than once per call: each create asks
// Docker Hub for the claude kit, and a create per call spent the account's
// pull rate limit within an hour of a first backfill. agentcore gives each call
// a fresh directory and session of its own, discards a sandbox a call broke, and
// [llm.Registry.Close] removes them all. The sandbox is what keeps
// ingested text, which is untrusted, away from the owner's files, and the
// sandbox's proxy is what supplies the Claude Code login: Hearsay never holds,
// reads or passes a model credential for this provider.
//
// It is host-only. It needs the `sbx` CLI, its daemon and a stored login
// (`sbx secret set`) on the machine running the process, so it does not work in
// the Compose or Helm deployments, and the registry refuses to be built where
// `sbx` does not answer (see [Provider.Completer]).
//
// Like the anthropic adapter it is thin. Retries, backoff, the per-attempt
// timeout, schema validation and token accounting are the registry's: what is
// here is one sandboxed call, a translation in each direction, and enough of
// agentcore's error vocabulary to say whether trying again could work. Costs
// the CLI reports are passed on as reported (its estimate at API prices, not
// what a subscription is billed) and no price is derived from tokens.
//
// Two settings the Messages API takes have no equivalent here, so a tier that
// sets them is refused: `temperature` (the CLI has none), and `base_url` and
// `api_key_env` (there is no endpoint to replace and no key to name). That is
// declared in [Provider.Capabilities], and checked by [llm.CanServe].
// `max_tokens` is the output-token budget, delivered to the CLI as
// CLAUDE_CODE_MAX_OUTPUT_TOKENS.
//
// No error this adapter returns carries a prompt, an answer or agentcore's
// result text: an error string reaches a log line, and a prompt in a log line is
// L1 text in a log line.
//
// docs/config.md documents the provider for operators. What it costs is
// startup time on a tier's first calls, subscription usage limits, no temperature, and
// host-only.
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

// DefaultTimeout is one attempt's timeout when a tier configures none. A call
// creates a sandbox before it asks the model anything, which is seconds on its
// own, so the registry's 60s default would time out calls that are working.
//
// The value is an estimate: nobody has timed a real sandbox start. It leaves
// room for the startup and a long answer, and stays inside the two minutes the
// distiller and the assertion worker bound a baseline call to, so a timed-out
// attempt is reportable before the caller's own deadline. Each
// [agent.Completion] reports its SandboxStartup, which is how to measure it.
const DefaultTimeout = 105 * time.Second

// Sandboxes is how many sandboxes one tier keeps at most, and so how many of
// its calls run at once; a process whose distill tier is claude-sbx distils
// this many documents at once. The assertion worker's calls are serialized and
// use one.
const Sandboxes = 4

// SandboxIdleTimeout removes a kept sandbox no call has used for this long.
// Each is a VM holding about a gigabyte on the owner's own machine, beside
// everything else it runs: a backlog keeps them warm, and between bursts they
// are given back. A burst after a quiet spell creates them again, a few
// creates rather than one per call.
const SandboxIdleTimeout = 5 * time.Minute

// SandboxMemory bounds each sandbox's VM. A tool-free turn needs Claude Code
// and nothing else; sbx's default is half the host's memory.
const SandboxMemory = "1536m"

// RegistryRateLimitWait is how long a call refused for Docker Hub's pull rate
// limit asks to be left alone. Hub says nothing of when its window rolls, and
// every attempt inside it is another request against the limit.
const RegistryRateLimitWait = 10 * time.Minute

// namePrefix prefixes the sandboxes this adapter creates, so that an operator
// listing `sbx ls` can tell whose they are.
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
// Sandboxes it fails, here, when the registry is built, rather than on the first
// artifact somebody ingests: this is the place the anthropic adapter refuses a
// missing API key.
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

// Embedder implements [llm.Provider]. The registry does not call it — it reads
// [Provider.Capabilities] first — but a caller building an adapter by hand gets
// the same answer rather than a nil client.
func (p *Provider) Embedder(llm.Build) (llm.Embedder, error) {
	return nil, fmt.Errorf("%s has no embedding model: point the embed tier at a provider that has one", llm.ProviderClaudeSbx)
}

// client is one tier's claude-sbx client.
type client struct {
	tier      llm.Tier
	model     string
	maxTokens int
	timeout   time.Duration
	sandboxes *agent.Completer
}

// Close implements [llm.Closer]: it removes the tier's sandboxes.
func (c *client) Close(ctx context.Context) error { return c.sandboxes.Close(ctx) }

// Complete implements [llm.Completer]: one sandboxed completion, one answer.
func (c *client) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	creq, err := c.request(req)
	if err != nil {
		return llm.Response{}, err
	}
	done, err := c.sandboxes.Complete(ctx, creq)
	if err != nil {
		// What agentcore said can quote the prompt or the answer, so neither
		// its text nor its error's is returned or logged: the log
		// line has the tier, the model, the class this adapter put the failure
		// in and the few fields agentcore gives that are not text.
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

// request translates a call. The tier's model, the caller's system prompt in
// place of the CLI's own, the schema if there is one and the output-token budget
// in effect all arrive as the agentcore request's own fields; nothing is
// embedded in the prompt.
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

// prompt is the conversation as the one user message the CLI takes. A single
// message is sent as it is. A longer conversation labels each turn with its
// role, oldest first, so the model can tell what it said from what it was told.
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

// label names a role in a flattened conversation.
func label(r llm.Role) string {
	switch r {
	case llm.RoleAssistant:
		return "Assistant"
	default:
		return "User"
	}
}

// response translates a completion back into terms no provider owns.
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
			// agentcore reports a missing answer as an error; this is the
			// same refusal for the case it did not, never an empty success.
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

// failure turns what agentcore returned into an [*llm.Error]. The order is
// agentcore's own: the usage limit, then the context's error, then a missing
// structured output, then everything else.
//
// None of the messages here is agentcore's: its errors carry claude's result
// text, which can quote the prompt or the answer. The attributes returned with
// the error are for the log and are the same kind: the class and fields agentcore
// gives that are an enum, a time or a window's name, never text.
func (c *client) failure(ctx context.Context, err error) (*llm.Error, []slog.Attr) {
	var limit *agent.UsageLimitError
	switch {
	case errors.As(err, &limit):
		// The subscription's usage limit rolls over at a time claude
		// reports. Waiting for it is what a retry is for, and the registry
		// does not cap a provider's wait: the caller's deadline ends it.
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
		// Docker Hub refused the sandbox's kit. Trying again at once is
		// another request against the same limit, so the registry waits,
		// and the caller's deadline ends the wait.
		e := c.err("Docker Hub's pull rate limit was reached creating the sandbox", true, agent.ErrRegistryRateLimited)
		e.Kind, e.RetryAfter = "registry_rate_limit", RegistryRateLimitWait
		return e, classOnly("registry_rate_limit")
	case errors.Is(err, context.DeadlineExceeded):
		// The attempt's deadline, sandbox startup included. A fresh attempt
		// could finish; the registry stops retrying if the caller's is gone.
		return c.err("the sandboxed call did not finish in time", true, context.DeadlineExceeded), classOnly("deadline")
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return c.err("the sandboxed call was cancelled", false, context.Canceled), classOnly("cancelled")
	case errors.Is(err, agent.ErrNoStructuredOutput):
		// A turn that ended without the answer: nothing says whether it ran
		// out of output budget or was refused, so it is not retried as though
		// a retry were known to help.
		e := c.err("the turn ended without structured output (it may have stopped at the output budget or declined)", false, agent.ErrNoStructuredOutput)
		e.Kind = "no_structured_output"
		attrs := classOnly("no_structured_output")
		var none *agent.NoStructuredOutputError
		if errors.As(err, &none) {
			// A CLI enum (success, error_max_structured_output_retries, ...).
			attrs = append(attrs, slog.String("subtype", none.Subtype))
		}
		return e, attrs
	default:
		// Anything agentcore or the CLI reported that this adapter cannot
		// classify is not known to be worth a retry. What it said stays out
		// of the error, and out of the log: only its Go type is logged.
		return c.err("the sandboxed call failed", false, nil), classOnly("other")
	}
}

// classOnly is the log attributes of a failure with nothing to say but its class.
func classOnly(class string) []slog.Attr { return []slog.Attr{slog.String("class", class)} }

// err builds this provider's error, with what the registry needs to decide
// whether to try again. It carries no request or answer content.
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

// The adapter satisfies the interfaces the registry builds from.
var (
	_ llm.Provider  = (*Provider)(nil)
	_ llm.Completer = (*client)(nil)
	_ llm.Closer    = (*client)(nil)
)

// quiet is the logger agentcore's runner is given. It passes on only a warning
// or worse, which leaves out the INFO lines agentcore writes around every call,
// and of a warning only what carries no text: agentcore attaches claude's own
// diagnostics (a "diagnostic" attribute) and the errors it met (an "err"
// attribute, which can hold a line of claude's output) to its warnings, and
// those can quote the prompt or the answer. A kept record has its message, which
// agentcore writes as a constant, and the attributes that name a call, a
// sandbox or a number; an "err" is replaced by its Go type.
//
// Hearsay's own debug line per model call, from the registry, says what a call
// cost; agentcore's would repeat it at a level nobody asked for.
func quiet() *slog.Logger {
	return slog.New(textless{slog.Default().Handler()})
}

// textless is a handler that drops every record below a warning and every
// attribute that can carry text from the rest.
type textless struct{ slog.Handler }

// safeAttrs are the attributes of agentcore's warnings that are names, paths or
// numbers of its own making rather than anything claude or a tool said. Anything
// else, including an attribute added in a later agentcore release, is dropped
// until this list is amended: an unknown attribute is not known to be safe.
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

// textlessAttrs is what of one attribute may be logged: itself when its key is
// a safe one, the Go type of an error when it is an "err", and nothing else.
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
