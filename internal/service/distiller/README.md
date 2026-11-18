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

# Outcome judgments

Jev decides every document's outcome, and the distiller needs it: `New` and
`Run` refuse without an `llm.Judgments`. The distill tier writes the summary,
the question, the change, the outcome sentence and the open questions. It does
not classify: its outcome sentence states the substance of what was concluded,
settled or put forward, without saying whether anyone agreed, so it reads true
under any kind.

The kind is four yes/no judgments asked in one request over the scrubbed
source: `outcome.work_finished`, `outcome.binding_agreement`,
`outcome.unagreed_proposal` and `outcome.still_open`, each journaled as its own
record. Code decides (`decideOutcome`): the first confident yes in that order
is resolved, decided, proposed or open, and no confident yes is none. A yes
below `llm.judgments.threshold` is a no. The kind has to fit the sentence:
none clears it, and resolved, decided or proposed with no sentence is none,
because a document whose model found nothing concluded, settled or put forward
enters no assertion. Open needs no sentence. Jev never writes prose.

A chat burst asks nothing: its containing thread owns the conclusion, so its
outcome, question and open questions are cleared and its kind is none.

A failed or invalid judgment is the job's error, as a failed model call is, and
the queue retries the job. Judgment inputs identify the document by its source
revisions, scope, ACL and deletion state (`JudgmentInputSpec.SourceBuilt`),
excluding the derived L1 row this operation writes, so a retry reuses every
judgment already committed for exactly those inputs and asks only the rest.
Interrupted attempts are never reused. A document whose sources change while
it is judged is superseded, which the provenance re-read finds; if that read
still finds it current, the job fails and is retried. A document an operator
deletion touched is judged but never journaled, because the journal refuses
inputs a deletion covers: its judgments are asked in full each time and it
records no observation.

The document transaction calls `l1.CommitJudgments` before any L1 write, with
all judgments and observations for the batch. A refusal is the write's error:
the transaction rolls back and the job is retried, so no outcome survives
without its committed records. The existing provenance re-read still drops
superseded answers before writing.

Each modeled document records a `distill` observation. Generative call counts
come from the shared per-context accounting seam, which includes registry
retries without racing concurrent jobs; judgment attempts and usage come from
the shared capability. Unknown tokens and provider costs stay unknown. The
stage measures per-document modeling and judgment through transaction entry;
segmentation and embedding are separate work, and their time/cost is not
imputed here. Recorded synthetic Jev answers under `testdata/outcome-judgments`
test the mechanics; they say nothing about Jev's accuracy.

A `work` source's `decision` is the one record distilled with no model call:
`distillDecision` builds its `decision` document from the event's structured
fields (`l1.BuildDecision`), writes it, and enqueues the job that places it
(`l2.EnqueueDecision`) in the same transaction. It makes no `distill` call,
embeds nothing and takes no part in the outcome pipeline, so a decision is
counted once, and a model that is down stops neither its document nor its
placement.
