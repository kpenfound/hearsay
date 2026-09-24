# 18. Operator deletion redacts L0 in place

- Status: accepted
- Date: 2026-09-23

## Context

L0 is append-only: nothing updates or deletes a row (migration 2,
`internal/l0`). A deletion at the source is a tombstone, which hides what it
covers while the rows stay. For a pasted secret, a departed employee's data or a
legally required removal, hiding is not deletion: the content is still in the
table and in every backup taken from now on. Issue #23 (Decisions 5 and 6)
approves one exception.

## Decision

`hearsay delete <selector> --reason <why> --apply --config <dir> --principal <human>`
deletes from Hearsay. The operator is a configured human principal. An unknown
id, an agent or a team, or a missing flag is refused before the database is
opened. There is no free-text operator and no fallback to the OS user.

One transaction:

1. walks provenance forward from the selector, the same walk the dry run
   prints;
2. writes a durable record in `l0_deletions`: id (`del_` and 32 hex digits),
   operator, reason, selector, time, the L0 events it covers, the L1 documents
   the walk found, and how many replays were dropped since;
3. writes Hearsay's own L0 event of kind `deletion` under source `hearsay`,
   naming the deletion, its operator and reason, with an ACL no reader holds,
   as an `audit` event has;
4. **updates the covered L0 rows in place**: `deletion` is set to the record's
   id, and the payload is replaced by a redaction marker. The row keeps its
   id, source, native id, kind, artifact, time, revision, target, ACL and
   change-feed position. The payload keeps only the fields that place the
   event (artifact, revision, target, thread, parent, part_of, base_kind, and
   the container's kind and id), because a tombstone for the artifact that
   arrives later still has to tell which conversation it was part of. The
   title, text, author, participants, mentions, links, paths, URL and native
   payload are gone;
5. enqueues a `distill` job for every L1 document the walk found.

If any step fails, none of them happened.

Every read hides a row with a `deletion` the way it hides one a source
tombstone covers. `Get` says `ErrDeleted` and names the deletion and operator,
never `ErrRetracted`: an operator deletion is not presented as the source's.
The running distiller rebuilds each queued document from what is still
visible, or deletes it when nothing is.

A connector replaying a redacted event (a backfill, a redelivery) writes
nothing, gets no error, and the replay is counted on the deletion record. A new
revision of the artifact has a new event id and is admitted, as it would be
after a tombstone.

## Consequences

- L0 is append-only except for this one update, and only `l0.Delete` performs
  it. Anything else that updates `l0_events` is a bug.
- Content is gone from the live table. It is not gone from backups, replicas,
  or bundles an agent already received (#23, Decision 11).
- Until the distiller has rebuilt a queued document, the old L1 text is still
  served. With a running distiller that is one job's latency. A deletion
  applied while no distiller runs waits for one.
- The L2 follow-up (stances resting on a rebuilt document) is issue #159. L2
  text redaction and `hearsay delete list|show` are issue #162.
