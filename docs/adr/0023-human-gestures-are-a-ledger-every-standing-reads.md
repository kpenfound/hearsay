# 23. Human gestures are a ledger every standing reads

- Status: accepted
- Date: 2026-09-24

## Context

Feature #24 lets a person ratify, demote or pin with one gesture in Discord,
and #25 does the same from GitHub. Decisions recorded on #24: a reaction acts
on every live stance drawn from the L1 document holding the message; a
demoted stance is served as `contested` until a person with authority
ratifies it or a newer stance supersedes it; removing the reaction undoes the
gesture; an unmapped or unauthorized person changes nothing; a pin anchors the
document in its scope. Issue #179 builds the part the two sources share.
Parsing a source's reaction or command, and answering it, are the connector
work items'.

L2's invariants still apply: a stance is never overwritten, and one scope's
writes are serialized on its `assert` key. The tier a read serves is computed
by `l2.Stand` and never stored. `TierInputs.Ratified` has existed since
standing was built, and nothing has filled it. `l2_pins` exists, and nothing
has written it. A source may deliver the same event twice. An operator
deletion ([ADR-0018](0018-operator-deletion-redacts-l0-in-place.md)) must
remove what a deleted event produced.

## Decision

**Gestures are an append-only ledger, `l2_gestures`.** Each row records the
L0 event the gesture came from, the principal, the action (`ratify`, `demote`,
`pin` or `undo`), the scope, the documents it named, and, for a ratify or a
demote, the stances it applied to. A pin row records the pins it made. An undo
row names the gesture it reverses and repeats what that gesture covered. The
event is unique, so a retried event returns the gesture already recorded and
writes nothing. A different gesture claiming the same event is refused. No
gesture row is changed after it is written.

**A ratify or a demote is resolved to stances when it is made.** It applies to
every live stance drawn from its documents. A live stance cites one of the
documents, has not been retired by a later reading of its own document, and
is not a withdrawal. A stance an agent asserted citing the document is the
agent's, and a gesture on the document does not apply to it. A newer stance
written later is not covered. That is how "until a newer stance supersedes
it" holds.

**Standing reads the gestures in force.** `Store.Assess`, the one path every
bundle, L3 view, `stance_history` and `hearsay topics` read goes through,
reads `Store.Corrections` for the stances it assesses and passes them to
`Stand` as `Ratified` and `Demoted`. The latest gesture in force on a stance
decides which of the two it is in. `Stand` serves a demoted current stance as
`contested`, whatever its evidence carries. A ratified one is served as
`ratified` unless a disagreement contests it, as before. A gesture is in force
until an undo reverses it or an operator deletion deletes its event.
Authority is checked when the gesture is recorded, not re-checked on each
read.

**A gesture is recorded as a topic operation is.** Only a configured human
the scope's `ratified_by.principals` names may make one (`CheckGesturer`, the
same rule as `CheckOperator`). The error wraps `ErrNotAllowed` and says why.
The documents must lie in one scope: the scope of their stances' topics, or
for a pin, the scope key of the artifact they distil. `RecordGesture` holds
that key as an `assert` job ([ADR-0020](0020-topic-operations-are-a-ledger-held-on-the-scope-key.md)),
using the target prefix `gesture-hold:`, which the assertion worker treats as
done if the hold outlives its process. It then decides and writes in the
transaction that completes the hold. A caller that already runs under the key,
such as the assert job for a gesture event that
[ADR-0022](0022-human-gestures-and-command-replies.md) plans, calls
`Store.ApplyGesture` in its own transaction.

**A pin writes `l2_pins`.** A pin gesture pins each document to every entity
in its L1 `scope` through `Store.Pin`, with the gesture's principal and time.
The first pinner stays the record. When a pin is undone or its event is
deleted, the document's pin is recomputed from the pin gestures in force. If
the row came from a gesture no longer in force, it is removed through
`Store.Unpin`, and the first pin gesture still in force pins the document
again. A pin that no gesture made is left alone. These recomputations take a
transaction-scoped advisory lock. An operator deletion takes the same lock for
its session before its repeatable-read snapshot begins, so its snapshot
includes every pin gesture recorded before it.

**An operator deletion takes a gesture out of force.** Deleting the event a
gesture came from voids it on every read, and `deletion.Apply` recomputes the
pins it made in the deletion's transaction. The ledger keeps the row. It holds
ids (events, documents, stances and entities) and no text, so nothing in it
has to be redacted. The deletion preview lists the gestures that would leave
force. An undo whose event is deleted still counts, because undoing only ever
removes an effect.

## Alternatives considered

- **Store the tier on the stance row.** This would overwrite a stance and
  store a tier that reads compute.
- **Record a gesture against documents and resolve it on each read.** A later
  reading of the document would inherit a ratification its author never
  gave, and a demotion would never end with a newer stance.
- **Re-check the principal's authority on every read.** Removing someone from
  `ratified_by` would silently undo everything they did. The gesture is a
  record of an act that was authorized when it was made.
- **Let a person's ratification settle a contest.** The existing standing
  rule says a person's ratification does not settle a disagreement, and #179
  does not change it.
- **Append undo rows when an event is deleted.** Nobody made those undos, and
  the read already has to exclude deleted events.

## Consequences

- Retries are idempotent by event, so a source needs a distinct event id per
  gesture occurrence. A Discord reaction removed and added again with the
  same id cannot be told apart from a retry (#180).
- A gesture's documents must lie in one scope, like a merge. Across scopes a
  caller makes one gesture per scope.
- Standing reads cost one more indexed query per assessment, over the stance
  ids they already hold.
- A gesture moves the bundle watermark, so a cached bundle is not served
  after one.
