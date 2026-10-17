# Chat burst gates

The distiller considers current messages in each native thread, each message's
replies on a source whose replies name that message as their thread, or 30
minute channel conversation, ordered by source time and artifact ID. A native
thread's root is not counted; a message that heads its replies is. A burst
requires all three gates:

- The conversation has at least **8** current messages.
- A contiguous run contains at least **3** messages by the same source identity.
- That author wrote at most **40%** of the conversation's current messages.

Equality passes each gate. An absent author breaks a run. A message by another
author breaks a run, even if the first author speaks again later. Runs never
overlap. The rarity gate counts all messages by an author, not only the run,
so a frequent participant's short aside does not qualify. Tombstoned messages
are absent; edits retain the message's original position.

Each qualifying run gets one `chat_burst` document whose ID is anchored to its
first message. It quotes only the run, folds only its ACLs, and links to that
first message. The thread owns any conclusion and L2 assertion; the burst
preserves only the tangent's distinct facts. Reprocessing reconciles burst rows
with current runs in the thread's write transaction. A run that drops below a
gate or changes anchor loses its old row.

# Meeting topics

A transcript takes a segmentation call before any per-topic distillation. The
model returns numbered line ranges and stable topic labels; code selects the
original source lines and deterministically extracts references. Repeated ranges
with the same label become one segment. The label's normalized hash anchors its
ID across source edits. A rename is a new identity, and reconciliation removes
the old row. Empty segmentation removes all current segments. One transaction
reconciles the set, so failed model calls leave the previous set intact. The
recorded, synthetic transcript and topic boundaries in `testdata/meetings/`
can be edited for prompt iteration without exposing real meeting content.

# Optional outcome judgments

The global boost setting makes each newly distilled document consult the shared
`llm.Judgments` capability after the baseline distill answer. `WithJudgments`
supplies that process dependency; it constructs no client. Disabled operation
makes no judgment calls. Without an outcome acceptance policy, judgments run in
shadow and leave the complete L1 document and assert enqueueing unchanged.

A policy must accept the confidence and exact resolved model version. When its
label agrees with the generator, the generator's outcome text stays. When the
labels disagree, the distill tier gets one additional structured request over
the same scrubbed source: it must explicitly confirm that the requested label
is supported and write outcome text for that label. Only that label/text pair
is adopted; the original summary, questions and other fields stay. A `none`
restatement requires empty outcome text; resolved, decided and proposed require
nonempty text. If the generator declines, fails, returns an invalid pair, or
runs out of time, the complete baseline pair stays. Jev never writes prose.
These checks enforce the reconciliation rule; neither model confidence nor a
well-shaped response proves semantic correctness.

Classification, lookup, durable attempt recording, provider retries/waits and
any generative restatement share **five seconds per document**. Baseline
model calls and writes use the live job context independently. Chat bursts
still have their outcome, question and open questions cleared: the containing
thread owns the conclusion, even under a qualified policy. No classification
can skip an L1 document. Control traffic is excluded before this path. The
existing provenance re-read still drops superseded answers before writing.

Judgment inputs identify the document by its source revisions, scope, ACL and
deletion state (`JudgmentInputSpec.SourceBuilt`), excluding the derived L1 row
that this operation writes. This permits restart reuse without weakening source
invalidation. Committed shadow and fallback decisions are never reinterpreted
under a later policy. For a committed accepted result, reuse the already stored
outcome label/text pair; do not ask either model to reinterpret it. Interrupted
attempts are never reused.

The document transaction calls `l1.CommitJudgments` before any L1 write, with
all judgments and observations for the batch. A refusal of any kind restores
all baseline documents in that batch before enqueueing assertions. The refusal
is logged with a sanitized reason, and distillation succeeds. No accepted
classification survives without its committed record. Journal preparation
failure likewise leaves baseline output; capture has a 100ms client deadline.
The journal bounds its commit statements and locks separately with its default
100ms limits and savepoint recovery.

Each modeled document records a `distill` observation for baseline, shadow or
accepted output. Generative call counts include the baseline and optional
restatement; judgment attempts and usage come from the shared capability.
The process integration supplies shared generative retry accounting; this
package does not difference process-wide counters across concurrent jobs. Unknown tokens and provider costs stay unknown.
The shared per-context accounting seam includes registry retries without racing
concurrent jobs. Provider-reported zeros retain presence; legacy zero token fields
without presence remain unknown. This stage measures
per-document modeling and optional work through transaction entry; segmentation
and embedding are separate work, and their time/cost is not imputed here.
Recorded synthetic fixtures under `testdata/outcome-*` test integration
mechanics; they qualify no production policy.
