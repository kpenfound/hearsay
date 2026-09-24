# 21. Reads follow the topic ledger

- Status: accepted
- Date: 2026-09-24

## Context

[ADR-0020](0020-topic-operations-are-a-ledger-held-on-the-scope-key.md)
records merges, splits and undos in `l2_topic_operations` and changes no
topic or stance row. What the operations in force make of a scope is the
ledger replayed over the rows. Issue #172 makes every read and the assertion
worker use that replay: `stance_history`, the current stances and their tier,
the L3 views and the bundle, `assert`, and topic matching.

A few cases have no single obvious answer. After a split, one stance's
supersedes pointer can name a stance that is now on another topic. A document
read again after a merge has its earlier reading on another row. A split's
topic has no row, but `l2_stances.topic_id` must name one before a new stance
can go there.

## Decision

**Every read replays the ledgers it touches.** A read first finds the scopes
whose ledger names a topic it reads, using the GIN index on `topics`. It then
replays those ledgers over the rows their operations name. A topic that no
operation names is read from its row, as before. The replay is
`ScopeState.arrange`, the same one `Decide` uses, so a person's operation and
a read never disagree about where a stance is.

**A topic merged away reads as the topic it went into.** `Topic`,
`StanceHistory` and matching give the same answer through either id. A stance
carries the topic it is on now. The merged topic keeps its own name and
opening document, is about everything its rows are about, and lists the
operations in force that shaped it (`Topic.Operations`, `stance_history`'s
`operations`).

**A split's topic is a topic in its own right.** The split names it and sets
its creation time. It has no opening document, so its live stances decide who
may read it, the same rule that applies when an opening document is deleted.

**A split's topic gets a row the first time a stance is written on it**
(`Store.Target`). The row carries the scope and satisfies the foreign key. A
read does not use its name, access list or opener. The row is a topic only
while a split that creates it is in force. Before that split, or after it is
undone, the stances written on the row belong to the topic the split took its
stances from, as that topic is now. So undoing a split also brings back what
was written on the split's topic in the meantime.

**Writes go to the topic as it is now.** `AppendStance` refuses a topic that
was merged away. It chooses the predecessor from every stance the topic holds,
whichever row each was written on, so a document read again still replaces its
own earlier reading. The assertion worker and `assert` resolve their topic
through `Target`. Matching runs its row queries as before, then maps each row
to its topic now, removes duplicates, and checks again whether the document's
readers may read that topic.

**Edges that cross topics are marked.** A supersession edge whose ends are now
on different topics is marked on both ends: `SupersedesTopic` on the stance
that supersedes, and `SupersededAcross` on the stance it superseded. A split
or an undone merge can leave such an edge. `stance_history` shows each end
only to a reader who may read the stance and topic at the other end.
Retirement ignores topic boundaries (`RetiredSQL` no longer compares topics).
A reading replaced by a later reading of the same document stays retired
wherever the later reading now is. Migration 00026 indexes `supersedes` for
this.

## Alternatives considered

- **Keep a materialized view of the effective topic of each stance.** Every
  operation and every stance write would then have to maintain the view in
  the same transaction. It would duplicate state that the ledger and the rows
  already determine.
- **Keep new stances off a split's topic until a person merges it back.** The
  worker would open a duplicate topic, and `assert` would refuse a topic id
  that `stance_history` serves.
- **Drop the foreign key on `l2_stances.topic_id`.** Every query that joins a
  stance to its topic's scope, such as the deletion repairs, would miss
  stances on a split's topic.

## Consequences

- A read of a scope with operations costs one extra read of its ledger and of
  the stance ids on the topics that the operations name. A scope with no
  operations costs one indexed probe.
- An undo restores the earlier reads exactly when nothing was written in
  between. A stance written while an operation was in force stays on the row
  it was written on: on the kept topic for a merge, and back on the source
  topic for a split.
- Matching finds a split's topic through the row it took its stances from,
  and through its own row once it has one. Join keys belong to topics, not to
  stances, so a split's topic is offered alongside its source whenever the
  source matches.
- Operator deletion does not follow the ledger yet. Its repairs and
  redactions work on rows, and reconciling them is a follow-up work item of
  #26.
