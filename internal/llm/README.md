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

See
[ADR-0005](../../docs/adr/0005-llm-provider-abstraction-with-three-model-tiers.md).
