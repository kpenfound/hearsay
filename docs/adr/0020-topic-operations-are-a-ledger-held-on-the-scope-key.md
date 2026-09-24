# 20. Topic operations are a ledger, recorded under the scope's serial key

- Status: accepted
- Date: 2026-09-24

## Context

Feature #26 lets a person merge two topics, split stances off one, and undo
either with one gesture. Wrong merges are the expected failure mode
(docs/design.md#l2-write-path), so an undo has to restore exactly what was
there. L2 has two invariants that constrain how: a stance is never
overwritten, only superseded; and one scope's writes are serialized, because
the assertion worker reads a scope's topics and then writes to them
([ADR-0007](0007-postgres-backed-job-queue.md)). An operation reads the same
topics and must not interleave with an assert job on the scope.

The serialization ADR-0007 built is a claim: a worker takes a pending job
whose key no running job holds. A person running `hearsay topics merge` is
not a worker, and cannot wait for one to get round to a job: the command has
to say whether the merge was recorded, or which operation stands in the way
of an undo.

## Decision

**Operations are an append-only ledger, `l2_topic_operations`.** A merge
records `[into, from]` and every stance on `from` at the time; a split
records `[topic, new topic]`, the new topic's name and the stances it moves;
an undo records the operation it reverses and repeats what that covered. No
operation writes a topic or a stance row. What the operations in force make
of a scope — which topics stand and which topic each stance is on — is the
ledger replayed in id order over the rows, leaving out what was undone. A
split's new topic has no `l2_topics` row; its id is derived from the split.

**An undo is refused when it is ambiguous.** Leaving an operation out of the
replay is only its reversal when nothing later in force covers one of its
topics. Otherwise the undo is refused and names those later operations, which
are undone first. An undo is not itself undone: to redo, make the operation
again. An operation is undone at most once.

**Only a configured human whom the scope's authority lets ratify by hand
(`ratified_by.principals`) may operate.** No agent, whatever its class.

**An operation holds its scope's serial key as an assert job.**
`queue.Client.Hold` writes an `assert` job for the scope that is already
running, under the same claim lock and at READ COMMITTED like the serialized
claim, and then waits for the jobs that were running on the key to finish.
From the moment the hold commits no claim takes the key, so a hold waits for
the job in flight and never for the backlog behind it. The operation is
decided and recorded in one transaction that also completes the hold job
(`CompleteIn`); if the hold's lease was lost the completion finds nothing and
the operation rolls back. A refused or failed operation completes the hold
outside it. A hold that outlives its process is reclaimed like any job, and
the assertion worker treats its target (`topic-operation:`) as done.

## Alternatives considered

- **Rewrite `topic_id` on the stances.** Undo would then need a record of the
  old values anyway, and it breaks the invariant that a stance is never
  overwritten.
- **Enqueue the operation as a job for the assertion worker to apply.** Uses
  the claim unchanged, but the person waits on a worker that may be down, the
  request has to be stored somewhere before it is decided, and a refusal has
  to be carried back to the command.
- **Poll until the key is free, then work under the claim lock.** Starves: a
  backfill keeps a key busy with a gap of one claim between jobs.
- **A separate advisory lock per scope.** The assert job does not take it, so
  it would serialize operations with each other and not with the jobs.

## Consequences

- Reads and assertion matching do not follow the ledger yet; that is the next
  work item of #26, which reads the replay this ADR describes.
- The ledger is the evaluation's record of merges and splits: who, when, what
  was covered, and whether it was undone.
- An `assert` job row can now be a hold rather than work. Queue depth by kind
  counts holds while they stand, which is milliseconds unless a job was
  running on the scope.
- A hold waits for a running job for at most its lease: a job whose lease has
  expired is not waited for, so a stopped worker does not block a person.
