# internal/l2

The graph: what it means. Entities, topics, stances, anchors and the edges
between them.

**Belongs here:** the entity hierarchy and its aliases, topics, stances with
their tier and evidence, supersession chains, anchors, and the recursive
queries over them. Human corrections — ratify, demote, pin, merge — land here
too, and are as first-class as anything the assertion worker wrote.

**Does not belong here:** the extraction prompts (the assertion worker owns
them), and anything derived that can be recomputed — that is `internal/l3`.

Two invariants to preserve: a stance is never overwritten, only superseded; and
one scope's writes are serialized, or supersession chains interleave and
corrupt. The one exception to the first is an operator deletion's redaction of
text that is no longer current (ADR-0019, below).

## Things to know before changing it

- **Alias candidates require a decision.** The distiller records one vote per chat
  document when it links a PR that touched code and its existing model answer
  names that code. Candidates retain both documents as evidence and intersect
  their current access lists on retrieval. Resolve uses configured and confirmed aliases; rejected candidates retain their row and accept no new votes.
- **A pin is a record, not a foreign key.** `l2_pins` names an L1 document id
  and whoever pinned it; the document is derived and may be distilled again, so
  the pin outlives it, and a pin whose document is gone is not served. Nothing
  here filters pins by reader — which pins a reader sees is `internal/l3`'s.
  Pinning again keeps the first pinner; the pin gesture is later work.
- **The tier a read serves is computed, never stored** ([Stand], `tier.go`).
  It is a pure function of the topic's live stances, the artifact class and
  source of their evidence as L1 holds it now, their recorded `changes` /
  `restates` judgements, the scope's authority policy and which stances the
  reader may read ([Access]), so a policy change takes effect on the next read.
  A stance the reader may not read never makes the topic contested for them. The `tier` on a row is `RecordedTier`: what the
  stance's own document carried under the policy when it was written. It is part
  of the stance's id and nothing serves it as the topic's tier. `contested` is
  only ever computed.
- **Disagreement follows the supersession path.** A judgement is recorded
  against the position the worker was shown, which is the chain head the new
  stance supersedes, so it labels that edge. Two stances disagree where a
  `changes` edge lies between them; a stance with no recorded judgement is not
  a disagreement, and position text is never compared.
- **Serialization is the queue's, and the store assumes it.** `AppendStance`
  reads the predecessor and writes the row in two statements. That is only safe
  because every job under one serial key runs alone (ADR-0007), and topics are
  only ever matched under the key they were opened under ([ScopeKey]). Matching
  across keys would need a different lock, not a different query.
- **A new reading of a document replaces that document's own stance.** A
  document that already holds a live stance on a topic — it was re-distilled,
  or re-run after a deletion — supersedes that stance, not whatever is newest
  on the topic, so one document holds at most one live stance per topic and a
  replacement does not point to a later document. Its judgement is separately
  made against the current position shown in the prompt. The replaced stance
  is *retired* (`RetiredSQL`, [Current]): never a topic's current stance, and
  never a predecessor again. A new document version writes a supersession even
  if it takes the same position.
- **L1 deletion repairs L2 on the topic's serial key.** The distiller enqueues
  an assertion job for each live stance that cited a deleted document. The
  worker supersedes it with the same position and surviving citations, or an
  explicit `evidence deleted` withdrawal when none remain. Withdrawals stay in
  history but never stand as a current position. The original citation ids
  remain on a withdrawal for provenance.
- **An operator deletion redacts text; a tombstone does not** ([RedactDeleted],
  [ADR-0019](../../docs/adr/0019-operator-deletion-redacts-superseded-l2-text.md)).
  The distiller records each document an operator deletion rebuilt in
  `l0_deletion_rebuilds`. A stance written before that rebuild, once it is
  superseded, has its position replaced by [Redacted]; a topic whose opening
  document the deletion deleted, and which no document still in L1 supports,
  has its name replaced. `redacted_by` names the deletion, and ids, evidence
  and `supersedes` edges stay. The sweep runs in the distiller's rebuild
  transaction and again in the store whenever a stance supersedes another,
  because a current stance is never redacted: it waits for its withdrawal or
  its document's next reading.
- **Otherwise supersession follows `stated_at`, the evidence's time.** A
  document read late supersedes the newest live stance before it and is
  superseded by nothing, so the chain forks; the later stance is never
  rewritten to point at it.
- **Idempotency has two layers.** `l2_asserted` records the version of each
  document read, compared for equality and never ordered, so a restart makes no
  model call. Topic and stance ids are derived from what produced them, and the
  store refuses a stance id that is not [StanceID] of its topic, document,
  position, document version and tier (for an asserted stance,
  [AssertionStanceID] of its topic and event). A deletion repair derives its
  id from the predecessor and surviving evidence. The primary key skips a
  retry of the same reading or repair, while a new version or tier records
  another stance even if the position is worded the same way.
- **An asserted stance is read from an event, not from what it cites.** A
  stance an agent writes through the API's `assert` call records its L0
  `assertion` event (`Stance.Assertion`); its evidence is the documents the
  agent cited. Its origin is the event, so a later reading of a cited document
  neither retires it nor supersedes it as its own (`RetiredSQL`,
  `predecessorSQL`); its id is [AssertionStanceID] of topic and event; and
  [Stand] ranks it as class `agent` from the event's source
  ([AssertedEvidence]), whatever it cites — citing a merged pull request does
  not lend an agent a merged pull request's authority.
- **Who may read a topic or a stance is decided from L1 as it is now**
  ([Access]): a topic by its opening document while it exists, then by a live
  surviving stance; a stance by every piece of its evidence, each still in L1
  and readable. The `acl` written on a topic
  or a stance is what its first document allowed when the worker read it; a
  second piece of evidence, an ACL re-sync (ADR-0013) or a retraction is not in
  it, so no read authorizes on it. A stance whose evidence is gone fails closed.
  Reach is decided from the same read.
  `Descendants` and `LinkedCode` are what a reach is expanded over
  (`principal.Reach`).
- **A topic is only offered to a document everyone who may read it may read the
  topic too** (`topicReadableBy`): its opening document's current access list,
  or a surviving live stance when the opener is gone, is public or carries
  every grant of the document's, compared without labels.
  Otherwise a private topic's name reaches a prompt about a public document and
  the stance it produces. Its current position is shown only where every piece
  of that position's evidence passes the same test
  ([Store.EvidenceReadableBy]); otherwise the topic is offered with no position
  recorded, and a stance the document adds to it records no judgement, since
  nothing was shown to judge against.
- **Join keys leave out people and code entities.** Everybody's documents name
  the same people and systems, and a key everything shares joins everything.
- **The hierarchy is merged, not accumulated** ([MergeHierarchy], ADR-0016).
  `code/`, then tracker hierarchy, then repository structure: the highest
  source that sets any parent for an entity replaces the others' parents, and
  an edge closing a cycle is dropped from the lower rank and logged. Repository
  structure is [NestByPattern], which compares path patterns and never expands
  them, so it reads no repository. Both are pure; seeding calls them. Tracker
  hierarchy is what L0's issues are `part_of` now ([PlacementOf]); between
  seedings the assertion worker places each item again as its issue changes
  ([Store.Place]), which re-merges the tracker's edges under one advisory lock
  rather than under a scope's serial key, because an edge can cross scopes.
- **Nothing unresolved is invented.** A tracker item is an entity only where a
  scope's tracker maps its repository, and a parent in a repository no tracker
  maps is no edge; a CODEOWNERS owner is a principal only
  where the identity mapping resolves it.
- **`resolve` hides path-like words from alias matching.** An alias is not
  matched inside `engine/server/write.go` or a link, and an alias shaped like a
  path (`acme/api#12`) only matches a whole word.

See [docs/design.md](../../docs/design.md#l2-the-graph).
