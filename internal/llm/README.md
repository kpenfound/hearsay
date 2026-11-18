# internal/llm

The model provider abstraction and the three model tiers: `distill`, `assert`,
`embed`. Those three names are a contract — config and the docs use
them verbatim.

**Belongs here:** the tier registry, the `Completer` and `Embedder` interfaces,
per-provider adapters in subpackages, structured-output handling, retries and
rate limiting, token accounting, and the fake that replays recorded fixtures.

**Does not belong here:** prompts. Each caller owns its own prompt — the
distiller owns the distillation prompt, the assertion worker owns the extraction
prompt — because a prompt is domain logic, not provider plumbing.

**Testing:** no test in this repository may make a real model call. Tests use
the fake and a recorded fixture; a test that would need a new fixture records it
deliberately and checks it in.

## How it fits together

```
config.Repo.LLM  ->  llm.NewRegistry(cfg, providers.All())  ->  Completer / Embedder
                          |                                          |
                          |  retries, backoff, Retry-After,          |
                          |  per-attempt timeout, token and cost     |
                          |  accounting                              |
                          `-- anthropic.New() / claudesbx.New() ----'
```

Two adapters ship, both completion-only, and neither has an embedding model:

- **`anthropic`** calls the Messages API with an API key. It is the shipped
  default, and the only one the Compose and Helm deployments can use.
- **`claudesbx`** (`claude-sbx`) answers through the owner's Claude Code login,
  with no API key: each call is agentcore's `Runner.Complete`, one tool-free
  `claude -p` turn in a Docker Sandbox created for that call, so ingested text
  never reaches the owner's files.
  It is host-only, and building a tier runs agentcore's `CheckSandbox`, which
  fails registry construction naming the tier, the provider and `sbx` where
  `sbx` does not answer. Its system prompt replaces the CLI's; a schema goes
  through `--json-schema` and comes back as `structured_output`; `max_tokens`
  is the CLI's output-token budget (`CLAUDE_CODE_MAX_OUTPUT_TOKENS`). A turn that
  ends without a structured answer or without text is a non-retryable
  `*llm.Error`, never an empty success, because agentcore does not say whether it
  was truncated or refused. The subscription's usage limit is a retryable
  `*llm.Error` with `RetryAfter` set to the time until the reset. Its package
  comment says the rest. The user-facing description is in
  [docs/config.md](../../docs/config.md#the-claude-sbx-provider).

- **The adapter is thin.** It makes one call, translates in each direction, and
  says whether trying again could work by returning an `*llm.Error`. Everything
  cross-cutting is the registry's, so there is one place that knows how a model
  call behaves and the distiller does not implement any of it.
- **A tier is built when the registry is, not on the first call.** An unknown
  provider, a missing credential, a provider with no system prompt or no
  structured output: all of them are errors from `NewRegistry`, because
  configuration is read once at startup and a process that cannot make a model
  call should not be serving.
- **The fake is the real registry with the network replaced.** `NewFake` builds
  through `NewRegistry` from the same `Config`, so a test exercises the same
  request validation, the same defaults, the same retry loop and the same
  accounting. A request nothing recorded is `ErrNoFixture`, which is what a test
  that would otherwise reach a provider gets.

## Things to know before changing it

- **Nothing provider-shaped may appear in `Request` or `Response`.** That is the
  leak the tier abstraction exists to stop: a caller that built a tool block would be a
  caller that has to be edited to add a second provider. Anthropic's tool call
  and `tool_choice` live in `anthropic/` and nowhere else.
- **Structured output is validated here, against the caller's schema**, before
  the answer is returned. `SchemaKeywords` is the JSON Schema this package
  understands, and a schema using anything outside it is **refused** rather than
  sent: a constraint that is passed to the model and not checked on the way back
  is one the caller would believe was enforced. Widening the subset means
  teaching `schema.go` to enforce the keyword, in the same change.
- **`const` and `enum` compare values, not spellings.** The schema is bytes
  somebody typed and the answer is a value the decoder produced, so both sides go
  through one canonical form (`canonicalJSON`): object keys sorted, strings
  escaped by one encoder, and every number in one spelling at every depth — `1`,
  `1.0` and `1e0` are one constant inside an object as much as on their own.
  Without that, a `&` in an enum member, or keys written out of alphabetical
  order, would refuse an answer that is exactly right, permanently, because a
  schema violation is not retried. Anything new that compares JSON here uses the
  same function; the fixture key does.
- **A provider says what it cannot take, and the registry refuses the rest.**
  `Capabilities.NoTemperature`, `NoBaseURL` and `NoAPIKeyEnv` are checked in
  `CanServe`, which `hearsay config validate` (`buildTier`) and `NewRegistry`
  both call, so the two cannot disagree. Only what a configuration *sets* is
  refused; a default never is. `claude-sbx` sets all three, so a tier of it with
  a `temperature`, a `base_url` or an `api_key_env` is an error naming the tier
  and the setting. `Capabilities.DefaultTimeout` is a provider's own per-attempt
  default, applied before `withDefaults`; `claude-sbx` takes 105s, because a
  call starts a sandbox first, and a configured `timeout` always wins.
- **Reported cost is provider-neutral, and never derived.** A provider that
  knows what a call cost puts it in `Response.ReportedUsage.CostUSD`, and the
  registry adds it to `Usage.CostUSD`, per tier, provider and model, exactly as
  reported. `Usage.CostReported` is what tells a **reported zero** (true, with a
  `CostUSD` of 0) from **no cost reported** (false). `anthropic` reports none, so
  its rows show `CostReported: false` and its token counts are what they were. A
  negative, NaN or infinite figure is not a usable report and counts as none. No
  price is ever computed from tokens. For `claude-sbx` the figure is the CLI's
  estimate of what the call would have cost at API prices, not what a
  subscription costs.
- **`Embedder` is a fixed interface, and it reports no tokens.**
  An adapter whose provider counts them implements `UsageEmbedder` as well, and
  the registry accounts through that; an adapter that does not is counted with
  no tokens against it rather than with a guess.
- **The token budget is part of a fixture's key.** A fixture recorded at one
  budget is not replayed at another, because it is a recording of a call that
  never happened. `FixtureKey` is exported so a recorder can name a file after
  the call in it.
- **An error here must be free of request content and of answer content.** A
  prompt is L1 text on its way to a model, a completion is what comes back, and
  an error string ends up in a log line — and, for a caller inside a queue
  handler, in `queue_job.last_error`. Errors here name the
  tier, the provider, the model, the field, the schema and the fixture key —
  never the text. A schema violation says what the schema allowed and what
  *kind* of thing came back (`not one of "decided", "proposed", found a
  string`), never the value; a number out of range prints the number, which is
  not text. **The one exception** is the name of a field a closed schema does not
  allow: it is what tells a prompt's author that the model wrote `outcome` where
  the schema says `outcome_kind`, so it is named, and bounded to the size of an
  identifier (`boundedField`).
- **There is no metric emission yet.** `Registry.Usage` is the read, tagged by
  tier, provider and model, with calls, failures, tokens and any reported cost,
  and it is where the per-tier metrics come from once `internal/telemetry` has a
  meter to report through.
- **A caller owes this package a deadline.** A provider's `Retry-After` is
  honoured as asked and is not capped, and the per-attempt timeout does not bound
  the wait between attempts, so a provider or gateway answering
  `retry-after: 3600` holds `Complete` for an hour per retry. That is deliberate
  — the provider knows its own rate limit better than a constant here does — and
  it is safe only while the caller's context has a deadline. A worker calling
  this **inside a queue handler** must still bound the call.
  The queue worker renews its lease while the handler runs and cancels the run
  if another worker has reclaimed it; the lease interval is not a total job
  deadline. The distiller and assertion worker bound model calls to two
  minutes; judgments have their own shorter bounds.

## Typed judgments

`Judgments` is Jev, separate from the three tiers and their provider catalogue.
The distiller and the assertion worker require it: `NewJudgments` refuses a
`JudgmentConfig` with no model, an environment with no key under its
`APIKeyEnv`, or no provider. Use one instance per process.

Callers own questions, minimized state and decisions. `Judge` batches
independent questions over shared state; `Choice`, `Score` and `YesNo` are
single-question conveniences. Every usable answer includes a complete normalized
distribution, confidence and the resolved model version. Hearsay's callers ask
only atomic yes/no questions, one `JudgmentTask` each, and make decisions from
the answers in code; `Choice` and `Score` remain for other callers.
`JudgmentConfig.Yes` is the one reading of an answer: a yes counts only at the
configured `Threshold` (default 0.8), and anything else is a no.

A failed call is a `JudgmentResult` with a sanitized `Failure` and its
accounting; `Err` turns it into a `*JudgmentError` the worker returns, so the
job fails and the queue retries it, as for a failed model call. Status reports
health, the last failure and counts for `/readyz`.

`providers.Judgments` constructs the Jev adapter, separately from
`providers.All`. Its wire contract follows the recorded agentcore System One
client: POST `/v1/systemone`, ordered choice criteria, score levels lowest first
and `noul` probabilities. Score results may be fractional within the requested
integer level range. Noul exposes `{yes: p, no: 1-p}` and confidence `max(p, 1-p)`;
that confidence is derived because the vendor supplies only the probability.
Choice and score confidence is vendor-reported. Rounded distributions within
the vendor's 0.02 tolerance are normalized; larger deviations are refused.

Defaults bound an attempt to 2s, a call including retries and waits to 5s, and
retries to one additional attempt. `MaxRetries: -1` disables retries. A caller
deadline shortens every bound, during attempts, retries and Retry-After waits.

Accounting uses independently nullable input tokens, output tokens and reported
USD cost. A reported zero remains known; missing values and overflowing sums
remain unknown. Invalid supplied accounting refuses the answer, retaining other
valid fields. Invalid-response markers are terminal even inside retryable errors.
Only sanitized failure constants leave the wrapper; provider error bodies,
prompts and answers never enter errors or logs. `FakeJudgmentProvider` scripts
slow, failing, rate-limited and invalid attempts through the same wrapper.


Worker stages use `ObserveCompletions` and `AccountCompletions` to collect logical
calls, all registry provider attempts (including retries) and reported consumption
through their own contexts. `Response.ReportedUsage` preserves explicit zero;
legacy zero token fields remain unknown. Failed attempts contribute their reported
usage, absent accounting and overflow remain unknown, and no cost is derived from
pricing. `Registry.Usage` continues to serve its process-wide usage view.
