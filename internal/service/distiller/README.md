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
