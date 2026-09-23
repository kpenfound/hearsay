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
corrupt.

## Things to know before changing it

- **This is the minimal graph.** Entities, topics and stances with supersession
  and two tiers: `ratified` for a merged pull request, `inferred` for everything
  else. Alias learning, authority per scope, `contested` and anchors are later
  work; nothing writes `contested` yet.
- **Serialization is the queue's, and the store assumes it.** `AppendStance`
  reads the predecessor and writes the row in two statements. That is only safe
  because every job under one serial key runs alone (ADR-0007), and topics are
  only ever matched under the key they were opened under ([ScopeKey]). Matching
  across keys would need a different lock, not a different query.
- **TierFor still uses the distiller's outcome classification.** It reads a
  resolved `pr` as merged. L1 now carries source merge state in
  `artifact_class`; the policy-based calculation in #115 will consume it.
- **A new reading of a document replaces that document's own stance.** A
  document that already holds a live stance on a topic — it was re-distilled,
  or re-run after a deletion — supersedes that stance, not whatever is newest
  on the topic, so one document holds at most one live stance per topic and a
  restatement never reads as a reply to a later document. The replaced stance
  is *retired* (`RetiredSQL`, [Current]): never a topic's current stance, and
  never a predecessor again. A new document version writes a supersession even
  if it takes the same position.
- **Otherwise supersession follows `stated_at`, the evidence's time.** A
  document read late supersedes the newest live stance before it and is
  superseded by nothing, so the chain forks; the later stance is never
  rewritten to point at it.
- **Idempotency has two layers.** `l2_asserted` records the version of each
  document read, compared for equality and never ordered, so a restart makes no
  model call. Topic and stance ids are derived from what produced them, and the
  store refuses a stance id that is not [StanceID] of its topic, document,
  position, document version and tier. The primary key skips a retry of the
  same reading, while a new version or tier records another stance even if the
  position is worded the same way.
- **Who may read a topic or a stance is decided from L1 as it is now**
  ([Access]): a topic by the document that opened it, a stance by every piece
  of its evidence, each still in L1 and readable. The `acl` written on a topic
  or a stance is what its first document allowed when the worker read it; a
  second piece of evidence, an ACL re-sync (ADR-0013) or a retraction is not in
  it, so no read authorizes on it. A document that is gone fails closed.
- **A topic is only offered to a document everyone who may read it may read the
  topic too** (`topicReadableBy`): its opening document's current access list
  is public, or carries every grant of the document's, compared without labels.
  Its current position is shown only where every piece of that position's
  evidence passes the same test ([Store.EvidenceReadableBy]); otherwise the
  topic is offered with no position recorded. Otherwise a private topic's name
  or position reaches a prompt about a public document and the stance it
  produces.
- **Join keys leave out people and code entities.** Everybody's documents name
  the same people and systems, and a key everything shares joins everything.
- **Nothing unresolved is invented.** A tracker item is an entity only where a
  scope's tracker maps its repository; a CODEOWNERS owner is a principal only
  where the identity mapping resolves it.
- **`resolve` hides path-like words from alias matching.** An alias is not
  matched inside `engine/server/write.go` or a link, and an alias shaped like a
  path (`acme/api#12`) only matches a whole word.

See [docs/design.md](../../docs/design.md#l2-the-graph).
