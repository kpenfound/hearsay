# 5. LLM provider abstraction with three model tiers

- Status: accepted
- Date: 2026-09-08

## Context

Hearsay calls models at write time only. `docs/design.md` is explicit that reads have no
model in the loop, so there are exactly three places a model is used:

- **L0 to L1**, in the distiller. High volume, one call per artifact, a bounded
  summarization task with a fixed output shape. Every event ingested pays for it.
- **L1 to L2**, in the assertion worker. Low volume (only docs whose `outcome_kind` is
  `decided`, `proposed` or `resolved`), and the hardest judgement in the system:
  extracting a stance and matching it to a topic. Getting it wrong shows up as a wrong
  merge that a human has to undo.
- **Embedding** L1 `text` for retrieval. High volume, no reasoning.

These have opposite cost profiles, and the right model for each will change more often
than the code around it. They are also not all served by the same vendor: Anthropic is
the first provider and has no embedding model, so the embed tier will point at a
different provider from day one.

The failure mode to avoid is provider shapes leaking into callers. If the distiller
builds an Anthropic tool-use block to get structured output, adding a second provider
means editing the distiller, and every prompt change becomes a provider question.

## Decision

A provider abstraction in `internal/llm` with three named model tiers.

| Tier | Used by | Character |
|---|---|---|
| `distill` | Distiller (L0 to L1) | Cheap, high volume, fast. A small model. |
| `assert` | Assertion worker (L1 to L2) | Stronger, low volume. Quality over cost. |
| `embed` | Distiller, on write; search, on query | Embeddings only. |

These three names are the contract. The repo scaffold (#2), config (#5) and the Dagger
module (#3) reference them verbatim.

**Provider and model per tier are configuration, not code:**

```yaml
llm:
  tiers:
    distill: { provider: anthropic, model: <small model>, max_tokens: 2048 }
    assert:  { provider: anthropic, model: <strong model> }
    embed:   { provider: <embedding provider>, model: <embedding model>, dimensions: 1024 }
```

Deliberately no model identifiers in this ADR. Naming one here would date the document
within a quarter, and the whole point of the decision is that changing it is a config
edit. The shipped default lives in the config defaults, where it can be bumped in one
line, and the tier characters above say what a replacement has to be.

**Callers see tiers, not providers.** The distiller asks for the `distill` tier and gets
something satisfying an interface along these lines:

```go
type Tier string

const (
    TierDistill Tier = "distill"
    TierAssert  Tier = "assert"
    TierEmbed   Tier = "embed"
)

// Registry resolves a tier to a client, per configuration.
type Registry interface {
    Completer(Tier) (Completer, error)
    Embedder() (Embedder, error)
}

type Completer interface {
    Complete(ctx context.Context, req Request) (Response, error)
}

type Embedder interface {
    // Embed returns one vector per input, in order.
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dimensions() int
}
```

`Request` carries a system prompt, an ordered list of messages, a token budget, and
optionally a JSON schema for structured output. `Response` carries the text or the
decoded JSON, a stop reason, and token counts. Nothing in either is provider-shaped.

**Structured output is part of the abstraction, not the caller's problem.** Callers
supply a JSON schema and get JSON back that validates against it. How a provider gets
there — a tool definition, a response-format parameter, or a retry on a parse failure —
is the adapter's business. This is the single largest source of provider leakage, so it
is the thing the interface is designed around.

**Cross-cutting concerns live in the abstraction:** retries with backoff on transient
errors, rate-limit handling, timeouts, and token accounting emitted as metrics tagged by
tier, provider and model (ADR-0008). The distiller and assertion worker do not implement
any of it.

Anthropic is the first provider. Adding a second means writing one adapter and no other
change.

## Alternatives considered

- **Call the Anthropic SDK directly and abstract later.** Fastest to start, and often
  right. Rejected because the embed tier already forces a second provider before the
  first milestone is done, so "later" is now; and because the abstraction is small
  enough that deferring it buys very little.
- **An off-the-shelf gateway or framework (LangChain-style, or an OpenAI-compatible
  proxy such as LiteLLM in front of everything).** Would give many providers for free.
  Rejected on two grounds: it collapses every provider onto one vendor's request shape,
  including structured output, which is where the shapes actually differ and where
  quality is lost; and for a self-hosted product it puts another service in the operator's
  deployment. An operator who wants a gateway can still point a tier at one, because the
  provider is config.
- **One model for everything.** Simplest config, and either wasteful (a strong model on
  every ingested Slack message) or bad at stance extraction. The volume difference
  between the tiers is at least two orders of magnitude, which is exactly when tiering
  pays.
- **More tiers, or a tier per call site.** Considered a fourth tier for meeting
  segmentation. Rejected: three tiers map to the three things the system actually does,
  and a tier per call site turns config into a second copy of the code's structure.
  Per-call-site overrides can be added within a tier if a specific prompt needs a bigger
  budget.
- **Local models (Ollama, llama.cpp) as the first provider.** Attractive for a
  self-hosted product handling private team communication, and this abstraction makes it
  an adapter rather than a rewrite. Not first, because the quality of the `assert` tier
  determines whether L2 is worth anything, and that has to be established against a
  strong model before anyone tunes it down.

## Consequences

- Testing works without a network. `internal/llm` ships a fake that replays recorded
  responses keyed by request, and the contributor rule is that no test calls a provider
  without a recorded fixture (#2). This is only cheap because callers depend on the
  interface, not an SDK.
- The `embed` tier's `dimensions` and the `vector(N)` column in ADR-0004 must agree.
  Changing the embed model to one of a different dimension is a schema migration plus a
  re-embed of every L1 row, not a config edit. Startup should verify the configured
  dimension against the schema and refuse to run on a mismatch rather than write
  garbage.
- Prompts live with the caller (the distiller owns its distillation prompt, the
  assertion worker owns its extraction prompt), not in `internal/llm`. The abstraction
  is transport and shape; the prompts are the product.
- Provider capability differences that cannot be hidden — a provider with no structured
  output, or no system prompt — surface as an error from the adapter at construction
  time, when config is loaded, not as a degraded call in production.
- Cost is observable per tier by construction, because every call goes through one place
  that knows which tier it is.
- Credentials are per provider, from the environment, never in the config repo, which is
  checked into git as GitOps (#5).
