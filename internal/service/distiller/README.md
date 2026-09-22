# Chat burst gates

The distiller considers current messages in each native thread or 30 minute
channel conversation, ordered by source time and artifact ID. The native thread
root is not counted. A burst requires all three gates:

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
