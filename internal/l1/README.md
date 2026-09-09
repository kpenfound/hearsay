# internal/l1

The document layer: what was said. One document per L0 artifact, flat rows in
one schema, rebuilt from L0 whenever the distillation changes.

**Belongs here:** the envelope and the per-kind bodies, the store, deterministic
reference extraction, the secrets and PII scrub that runs before `text` is
written, and hybrid search over the table (embeddings plus full text).

**Does not belong here:** the prompts that produce a document — those belong to
the distiller in `internal/service/distiller`, which owns its own prompt
(ADR-0005). Entities, topics and stances are `internal/l2`.

Distillation is a write-time LLM call. Reads never call a model.

See [docs/design.md](../../docs/design.md#l1-distilled-documents).
