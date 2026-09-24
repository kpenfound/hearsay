# 19. Operator deletion redacts superseded L2 text in place

- Status: accepted
- Date: 2026-09-24

## Context

An operator deletion redacts L0 in place and re-distils the L1 documents that
depended on it ([ADR-0018](0018-operator-deletion-redacts-l0-in-place.md)).
L2 follows L1: a stance whose document is deleted is superseded by a
withdrawal, and a stance whose document is re-distilled is superseded by the
next reading of it (#159). Supersession keeps the old row, which is the point
of it — `stance_history` shows what changed — but a stance's position and a
topic's name are model text read from the documents, and the old row still
holds what was read from the deleted content. L2 has one invariant that
matters here: a stance is never overwritten, only superseded. Issue #23
(Decisions 7 and 9) asks for the text to go, the history to stay, and a
record of what each deletion rebuilt.

## Decision

The distiller records each document an operator deletion queued in
`l0_deletion_rebuilds` — re-distilled or deleted, and when — in the
transaction that rebuilt it. Only the first rebuild after the deletion is the
deletion's. A source tombstone records nothing.

Two L2 texts are then **updated in place**, and `redacted_by` names the
deletion:

1. **A superseded stance's position**, where the stance was written before the
   deletion rebuilt one of its documents. It is replaced by `[redacted]`. A
   stance still current keeps its text until something supersedes it — a
   withdrawal, or the next reading of the re-distilled document — because a
   current position is never overwritten. A withdrawal's own
   `evidence deleted` is left alone.
2. **A topic's name**, where the deletion deleted the topic's opening document
   and no stance on the topic cites a document still in L1. A topic that a
   surviving document supports keeps its name.

Ids, evidence, `supersedes` edges, tiers and times are untouched, so history
still records the withdrawal. The check runs in the distiller's rebuild
transaction, for stances already superseded, and in the store whenever a stance
supersedes another, for stances that were current at the rebuild.

A stance whose surviving evidence replaces it keeps its position, as #159
decided. Only an agent's assertion cites more than one document, and its
position is the agent's words rather than text read from the one deleted.

`hearsay delete list` and `hearsay delete show <id>` read the record: what the
deletion covered, what became of each queued document, the stances and topics
it redacted, and a status that is `complete` once every document is rebuilt
and no `distill` or `assert` job on what it touched is still to run.

## Consequences

- L2 is superseded, never overwritten, except for this redaction of text no
  longer current. Anything else that updates `position` or `name` is a bug.
- A current stance holds its text until it is superseded. A re-distilled
  document whose new version asserts nothing leaves its old stance current,
  and so unredacted, until something supersedes it.
- The status is read from the queue, not stored. A later job on a document the
  deletion rebuilt, for a reason of its own, shows the deletion as rebuilding
  until it runs.
- Other L2 text read from the deleted content — alias candidates' names, for
  one — is not redacted.
