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

# Conversation topics

A `conversation` source's messages (`conversation.message`, thread = Kern's
conversation id) are distilled as the transcript is, but from many events. The
pump enqueues one job per conversation: a message's target is the
document id of its thread, so a burst of messages costs one job under the
usual debounce. A job for a conversation reads the current message revisions
that share the thread, orders them by when they were said (then artifact id),
and numbers them as one line each, `Owner`, `Owner (interrupted)` or `Kern`,
with the words on one line. A segmentation call returns numbered ranges and
topic labels; code selects the original lines. Ranges with one label become one
`conversation_segment` document, whose id is anchored to the normalized label,
so a topic that resumes later is one document and a rename is a new one. Each
document's L0 references are the message events whose lines it quotes and no
others, its ACL is folded from them (the owner alone), its class is `dm`, and
the interrupted mark is in the line that quotes it. A message no range takes
is cited by none. Documents whose lines did not change are not asked of the
model again and write no row.

A job for one of a conversation's topics, which is what an operator deletion
queues, rebuilds the whole conversation, because a deleted message can sit in
several topics and moves where the others begin.

**Only the owner's own words become the owner's stance.** The rule lives in
`conversation.go` and nowhere else. A line is the owner's only if the source
marked it the owner's role *and* authored it as the owner; anything else is
Kern's. The distill call cites, in `outcome_lines`, the Owner lines its outcome
sentence rests on; the sentence is dropped unless it cites at least one and
every one it cites is an Owner line (`ownerOutcome`). A document with no
outcome sentence is never decided, proposed or resolved, so it enters no
assertion, whatever Jev answers. Jev judges the outcome over the owner's lines,
with Kern's lines apart as context its questions say never count
(`ownerView`). The assertion worker reads a stance's position from
`l1.Document.StanceText`, which for a conversation segment is the outcome
sentence alone, not the summary, which also describes what Kern said. The L2
reads already authorize a stance on every piece of its evidence, so the stance
is the owner's alone in a topic that also holds evidence others may read.

**Completion record.** One transaction publishes a conversation: it reads the
inputs again, and only if they are still the revisions the attempt read it
commits the judgments, removes the topics that no longer exist, writes the
documents and writes the record, `l1_conversation_distillations`: the event id
of every message revision read (an event id names one revision) and the
documents produced, which is none when the segmentation selected nothing. A
failed or stopped attempt writes nothing. An attempt whose inputs changed under
it (a message rewritten, or one that arrived) writes nothing either, because
documents built from the old read are not what the newer messages say and a
record would name revisions no document was built from, and it enqueues
another job so the newer message is distilled afterwards. Publishing the same
inputs and documents again changes no row.

The record is how a message is known to have been processed; job state and
documents cannot say so, because the queue records no inputs and a segmentation
that quoted nothing leaves nothing behind. Read it through `l1.Store`:
`ConversationDistillationsReading(ctx, eventIDs)` answers, for each event id a
published distillation read, that distillation (`Inputs`, `Documents`), and
leaves out an id none read, which is `pending`: not yet distilled, rewritten
since, or not a message. `ConversationDistillationOf(ctx, source, thread)` reads
one conversation's. The API's `coverage` call (`l1.Store.Coverage`) consumes
it, with the current documents' L0 refs and L0's deletions. A message is
`covered` or `excluded` only by a distillation that read its current revision's
event id (`ConversationDistillation.Read`); whether a document of it quotes the
message is the current documents' L0 references. A conversation too long for one segmentation prompt fails its job
and records nothing, so its messages stay `pending`.

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
without its committed records. The provenance re-read drops superseded answers before writing.

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
