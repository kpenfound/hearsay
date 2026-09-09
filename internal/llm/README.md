# internal/llm

The model provider abstraction and the three model tiers: `distill`, `assert`,
`embed`. Those three names are a contract (ADR-0005) — config and the docs use
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
                          |  per-attempt timeout, token accounting   |
                          `------------- anthropic.New() ------------'
```

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
  leak ADR-0005 exists to stop: a caller that built a tool block would be a
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
- **`Embedder` is the interface ADR-0005 fixes, and it reports no tokens.**
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
  handler, in `queue_job.last_error` (ADR-0007, ADR-0008). Errors here name the
  tier, the provider, the model, the field, the schema and the fixture key —
  never the text. A schema violation says what the schema allowed and what
  *kind* of thing came back (`not one of "decided", "proposed", found a
  string`), never the value; a number out of range prints the number, which is
  not text. **The one exception** is the name of a field a closed schema does not
  allow: it is what tells a prompt's author that the model wrote `outcome` where
  the schema says `outcome_kind`, so it is named, and bounded to the size of an
  identifier (`boundedField`).
- **There is no metric emission yet.** `Registry.Usage` is the read, tagged by
  tier, provider and model, and it is where ADR-0008's metrics come from once
  `internal/telemetry` has a meter to report through.
- **A caller owes this package a deadline.** A provider's `Retry-After` is
  honoured as asked and is not capped, and the per-attempt timeout does not bound
  the wait between attempts, so a provider or gateway answering
  `retry-after: 3600` holds `Complete` for an hour per retry. That is deliberate
  — the provider knows its own rate limit better than a constant here does — and
  it is safe only while the caller's context has a deadline. A worker calling
  this **inside a queue handler holding a lease** (ADR-0007) must bound the call
  to less than that lease, or its job is reclaimed and re-run underneath it. If a
  cap belongs anywhere, it belongs here as a tier parameter; until a caller needs
  one, the obligation is the caller's and is written down.

See
[ADR-0005](../../docs/adr/0005-llm-provider-abstraction-with-three-model-tiers.md).
