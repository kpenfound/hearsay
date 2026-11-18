# 32. A second completion provider runs Claude Code in a sandbox per call

- Status: accepted
- Date: 2026-11-18
- Extends: [LLM provider abstraction with three model tiers](0005-llm-provider-abstraction-with-three-model-tiers.md).
  Nothing in 0005 is superseded: the three tiers, the one-adapter-per-provider
  pattern and the registry's ownership of retries and accounting stand.

## Context

ADR-0005 shipped one completion provider, `anthropic`, and it needs an API key.
That is the right default for a deployment, and it is a barrier for one person
trying Hearsay on a machine where they are already logged in to Claude Code with
a subscription. They have a model they can use and nothing to put in
`ANTHROPIC_API_KEY`.

Running the Claude Code CLI to answer a tier is the obvious route, and the
dangerous one. What Hearsay sends to a model is Slack, GitHub and Discord text
that somebody else wrote. A CLI turn that runs on the owner's machine, in
Hearsay's working directory, with the CLI's usual tools and settings, hands that
text to an agent that can read files, run commands and follow instructions it
finds in what it reads. The text is untrusted, and the files are the owner's.

[agentcore](https://github.com/kpenfound/agentcore) already runs Claude Code
inside Docker Sandboxes (`sbx`). It gained a tool-free completion call for this
(`Runner.Complete`, with `Runner.CheckSandbox` to ask whether `sbx` answers): one
`claude -p` turn ([headless mode](https://code.claude.com/docs/en/headless)) in a
sandbox created for the call and removed however it ends, with no built-in
tools, no MCP servers, no setting sources and an empty temporary folder as its
only mount. Its proxy supplies the stored login, so no credential is passed.

## Decision

Add `claude-sbx` as a second completion provider, one adapter subpackage under
`internal/llm`, in the provider catalogue beside `anthropic`. An operator picks
it per tier (`provider: claude-sbx`, `model: <id>`) on `distill` and `assert`.
The shipped default stays `anthropic`.

**Why a second provider at all.** It needs no API key: Hearsay never holds,
reads or passes a model credential for it, and a `claude-sbx` tier reads no
credential variable.

**Why a sandbox per call.** Each `Complete` is one agentcore call in its own
sandbox, so the untrusted text of one artifact reaches a turn that has no tools
and no files but an empty folder, and nothing it did survives to the next call.
Reusing a sandbox across calls would be faster and would let one artifact's text
affect the next call's turn, so each call gets its own. Pooling is a separate
decision, to be taken on a measurement, if startup comes to threaten the bounds
below.

**Why build on agentcore rather than shell out to `claude`.** The call gives
what a prompt cannot. The caller's system prompt *replaces* the CLI's own rather
than being appended to it, so the distillation prompt is the whole instruction.
A schema goes through the CLI's `--json-schema` and comes back as a
schema-checked `structured_output`, so Hearsay does not embed a schema in a
prompt and parse JSON from reply text. The output-token budget reaches the CLI as
`CLAUDE_CODE_MAX_OUTPUT_TOKENS`. Token counts, cost, the answering model and
failure classes (a usage limit with its reset time, a timeout, a missing
structured answer) come as typed values. Hearsay's registry still validates the
answer against the caller's schema, as for every provider.

**Shape.**

- The adapter makes one call and translates in each direction. Retries,
  backoff, the per-attempt timeout, schema validation and accounting stay in the
  registry (ADR-0005). A usage limit is a retryable rate limit whose wait is the
  time until the reported reset, bounded only by the caller's deadline, which the
  distiller and the assertion worker set at two minutes.
- Settings it cannot honour are refused when configuration is read, by `hearsay
  config validate` and registry construction through the same check:
  `temperature`, `base_url` and `api_key_env`. `max_tokens` is honoured. Only
  what a configuration sets is refused, never an inherited default.
- It has no embedding model, so `embed` refuses it as it refuses `anthropic`.
- Building a `claude-sbx` tier runs `CheckSandbox`, so `distiller`,
  `assert-worker`, `api` and `all` refuse to start where `sbx` does not answer,
  with a message naming the tier, the provider and `sbx`. `hearsay config
  validate` does not run it and works without `sbx`.
- The per-attempt default is 105s rather than 60s: a call creates a sandbox before
  it asks the model anything, and 105s still fits the two-minute bound so a
  timed-out attempt is reportable. It is an estimate, because nobody has timed a
  real start; a configured `timeout` wins.
- The registry gains provider-neutral reported cost. A cost a provider reports is
  summed per tier, provider and model in `Registry.Usage` exactly as reported, a
  reported zero stays distinct from none, and no price is derived from tokens.
  `anthropic` reports none and shows none. For `claude-sbx` the figure is the
  CLI's estimate at API prices, not what the subscription costs. A negative, NaN
  or infinite figure is treated as no cost reported.
- agentcore is a direct dependency, pinned by commit as a Go
  [pseudo-version](https://go.dev/ref/mod#pseudo-versions): no release tag, no
  `replace` directive, no vendored copy.

## Consequences

**Startup time.** Every call creates a sandbox, which is seconds before the
model is asked anything. That lengthens each distill and assert attempt, and is
why the default timeout is longer. It is a throughput cost for a high-volume
tier.

**Subscription usage limits.** The calls draw on the owner's subscription, which
has usage limits an API key does not. A limit that resets later than the
caller's deadline fails the call rather than waiting for it.

**No temperature.** The CLI has no temperature control, so a `claude-sbx` tier
cannot set one, and a tier moved from `anthropic` has to drop it.

**Host-only.** It works only where the processes run directly on a machine with
`sbx`, its daemon and a stored login. A container has neither and cannot reach a
sandbox host, so `claude-sbx` is not available in the Compose or Helm
deployments, which stay on `anthropic` ([deploy.md](../deploy.md)).

**A dependency to keep current.** The adapter follows agentcore's API at the
pinned commit, so a change there is a re-pin and an adaptation here.

**Cost figures are estimates.** What `Registry.Usage` shows for `claude-sbx` is
useful for comparing tiers and misleading as a bill.

## Alternatives considered

**Run `claude -p` on the host.** Simplest, and it gives untrusted text a turn
with the owner's files and tools within reach. Rejected for that reason alone.

**Embed the schema and instructions in the prompt and parse JSON out of the
reply.** Gives no replacing system prompt and no schema-checked answer, and
turns every format slip into a retry. Rejected.

**One sandbox for many calls.** Faster, and carries state from one artifact's
text to the next call. Deferred until a measurement shows startup is the problem.

**Make `claude-sbx` the default.** It is host-only and needs a login, so the
Compose and Helm deployments would break. The default stays `anthropic`.
