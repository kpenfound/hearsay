# 31. Typed judgments are an optional write-path capability

Date: 2026-10-17

Status: Accepted

## Context

Stance comparison, topic matching and document outcome classification are bounded
semantic questions. Generative extraction remains useful without a second model.
TypeSafe's Jev (System One) offers typed answers, but its quality on a team's
communication is an evaluation hypothesis. Confidence is not validated correctness;
typed output constrains shape, not truth. Negation, paraphrases, ambiguous topic
boundaries, proposal versus decision, adversarial source text and domain-specific
language remain failure cases to measure. The vendor snapshot is not evidence of
quality on a deployment's data.

## Decision

Add `llm.Judgments`, separate from `Completer` and `Embedder`, for choice, rubric
score and yes/no probabilities. ADR-0005's three tiers remain unchanged: `distill`,
`assert` and `embed` still produce summaries, outcome text, extracted positions,
new topic names and embeddings. Callers own questions and adoption; the shared
capability owns transport, retries, deadlines, accounting and cooldown.

`llm.boost.enabled` is the only enable switch, off by default. An absent or disabled
boost constructs no Jev client and reads no Jev credential. An enabled boost reads
an environment reference (`api_key_env`, default `HEARSAY_JEV_API_KEY`). Missing
credentials or provider failure degrade the capability while services start and
baseline work continues. One instance per process is passed to both workers in
`hearsay all`, sharing cooldown. Separate service processes have separate cooldowns.
Worker `/readyz` includes `judgments.enabled`, `healthy`, `last_fallback`,
`observations`, `attempts` and `fallbacks`; optional degradation never changes
baseline readiness.

All integrations run in shadow until their task has a qualified policy. Policies
bind a threshold to an exact resolved model version and question variant, including
the broader topic pool as a separate variant. Configure the requested model
explicitly; no model is auto-selected. Resolved versions are recorded. A version
mismatch falls back; an upgrade never reinterprets historical consumed judgments.
Use human-reviewed tuning and final held-out splits and operator-supplied limits
through [the evaluation procedure](../jev-evaluation.md). Ship no qualified policy
or default quality limits. Automated checks use recorded fixtures and fake providers.

Keep the complete baseline extraction. Uncertainty, abstention, invalid responses,
timeouts, rate limits, outage cooldown and exhausted budgets retain it. Topic
replacement requires a coherent comparison to the replacement's readable current
position. None-fit never opens a topic. Outcome disagreement requires the distill
tier to confirm support and restate coherent outcome text; otherwise keep the
original label/text pair. Always write the L1 document.

Commit judgments with their consumer output in the same transaction. The journal
validates the entire batch against effective source, membership, candidate, ledger,
ACL and deletion inputs. Any refusal, including storage failure, statement/lock
timeout or changed inputs, restores the complete baseline document or assertion
job, with no partial boosted batch. Commit-time guards close the validation race.
Reuse only committed records whose inputs still match, retaining their original
policy and resolved version. Records keep identities, labels, probabilities and
accounting, not source or completion prose, and follow ACL and deletion rules.

One assertion job shares `assertion_job_budget` (default 5s, maximum 30s) across
stance questions, broader-pool reads/shortlisting, topic matching, replacement
comparisons, optional re-extraction, retries and Retry-After waits. The deadline
starts before the first optional stage and never resets per assertion or task.
Remaining questions fall back on exhaustion and the complete baseline is written.
Thus optional work adds at most this budget to scope-key holding; baseline model
and database time, queue scheduling and later jobs' own execution remain separate.
Human gestures wait on that same key; the lease is renewed while jobs run and is
not a time bound. Outcome classification and any restatement share a fixed 5s
per-document optional budget. Baseline writes never use an expired optional context.

Bundle assembly, search, standing, authorization, command/gesture application and
deletion have no judgment dependency. Human authority, ratification, deterministic
supersession, merges/splits, privacy and deletion policy stay in their existing layers.

## Consequences

Reported quality and complete pipeline cost must both justify adoption. Per-stage
accounting includes generative calls and all provider attempts, including retries,
and logical judgment requests including zero-attempt fallbacks. Reused results
add no new requests. Missing or overflowing tokens/cost stay unknown; a reported
zero stays zero. No price-based savings claim is supported. Stage observations
cover modeling and optional work; segmentation, embedding and work outside those
stage boundaries are not imputed as zero. Provider cancellation is part of the
adapter contract; a non-cooperating adapter cannot satisfy the time bound.
