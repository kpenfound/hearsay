# 24. Discord commands are applied by the interaction adapter

- Status: accepted
- Date: 2026-09-24
- Supersedes: the clause of [ADR-0022](0022-human-gestures-and-command-replies.md) that has the assertion worker interpret Discord slash commands

## Context

[ADR-0022](0022-human-gestures-and-command-replies.md) records every gesture
as an L0 event. It has the assertion worker's change-feed follower interpret
each gesture under the scope's `assert` key, and lets the Discord interaction
adapter in the API defer its answer while the worker completes. The answer
to `/hearsay pin` or `/hearsay merge` has to state the result or the refusal
(#181). The adapter would have to wait on a job it did not run and then learn
from the ledger why nothing was written. An unmapped or unauthorized person
leaves no row to read, and a refusal written as a durable outcome is not
there yet.

[ADR-0023](0023-human-gestures-are-a-ledger-every-standing-reads.md) gives
`l2.RecordGesture` to callers outside the worker. It holds the scope's serial
key as an `assert` job does, keys the gesture by its L0 event, and refuses a
person the scope's `ratified_by.principals` does not name. `l2.Operate` does
the same for a merge ([ADR-0020](0020-topic-operations-are-a-ledger-held-on-the-scope-key.md)).
`hearsay gestures` and `hearsay topics` already call both.

## Decision

**The interaction adapter applies a Discord command itself.** After it
verifies the request, it records the command as an L0 `command` event under
the Discord source, using the interaction's id. It does this only in a
channel the source's allowlist admits. It then maps the invoking user to a
configured human, reads as that person through the view `hearsay topics`
reads through, and calls `l2.RecordGesture` for a pin (keyed by the command
event) or `l2.Operate` for a merge. The answer is what that call returned or
why it refused. When the work outlasts the deadline, the adapter defers the
ephemeral answer and edits it in with the interaction token once the call
returns.

The serialization, idempotency and authority rules of ADR-0022 are unchanged.
The assertion worker still interprets reactions and GitHub `/hearsay`
comments. It does not interpret a Discord `command` event, and the distiller
reads no document from one.

## Alternatives considered

- **Enqueue a `gesture:` job and wait on it.** The answer would have to be
  rebuilt from what the ledger holds. That cannot tell an unmapped user from
  an unauthorized one, or either from a job still waiting behind the scope's
  key.
- **Answer "received" and apply later.** A person would get no result and no
  refusal, and the command answer exists to give one.

## Consequences

- A command's gesture or operation is written by the API process. It holds
  the same scope key the worker's jobs do, so nothing interleaves with it.
- A command whose work outlives its process is not retried. The person sees
  no answer and can run it again, and a pin run again is a new gesture.
- The API needs the Discord bot token to register commands, and the public
  key to verify them.
