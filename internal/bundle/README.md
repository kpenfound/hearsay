# internal/bundle

Assembly of the context bundle — the primary read, and the thing the whole
system exists to produce.

**Belongs here:** section assembly (directive, scope, anchors, stances, recent,
open questions, conflicts, handles), the token budget and the drop order,
dedupe by kind before cutting by recency, conflict computation, and the
permission filtering that decides what a given principal sees.

**Does not belong here:** transport. Serving the bundle over MCP or HTTP is
`internal/service/api`. No model is called on this path — a bundle is a
structured lookup, and query-time synthesis is a non-goal.

Rules worth restating because they are easy to break: `recent` is capped by
count and never filtered by age; ratified stances never drop; `anchors` are not
subject to the `recent` cap; no code content, ever.

## Things to know before changing it

- **A scope is an entity id** — `tracker:github:acme/api#12`, `code:acme/api`. The
  entities a bundle is about are that one and the ones its own document is
  about; the ancestors of those are where inherited stances come from.
- **Every time is absolute.** The design writes `when: 23d`; a relative age would
  make the same scope and principal produce a different bundle every minute,
  which breaks "cacheable". A consumer computes the age.
- **The budget is estimated from bytes** ([BytesPerToken]), not a tokenizer: the
  bundle must not depend on which model a deployment names. It drops from the
  bottom — open questions, then `recent`, then anchors, then inherited stances
  whatever their tier, then the scope's own stances that are not ratified, each
  last first — and can end over budget, because the scope's own ratified
  stances never drop.
  "Ratified stances never drop" is read as the scope's own: a tracker item
  inherits every topic in its repository, and holding every merged pull
  request's stance would make the budget no limit at all. A code entity asked
  for as the scope owns every topic in it, so that bundle is the one that can
  still grow past the budget.
- **A directive comes from L0, never request text.** Assembly reads the current revision and its ACL for every request; the requested id only identifies the artifact. The block is verbatim and cannot be trimmed.
- **[Encode] is the one encoding.** Every interface serves its bytes verbatim;
  that is what "identical output regardless of which interface asked" rests on.
- **Anchors drop out of layout order.** They sit above `stances` in the
  bundle, as the design draws it, but drop after `recent` and before any
  stance. An anchor is a pointer the agent can still find with `search`; a
  stance is the team's position, which it cannot recover as cheaply. They drop
  after `recent` because the design document is what the activity is about.
- **Anchors are pinned, inferred, then defaulted** (`l3.Views.Anchors`): the
  scope's pins first pinned first, then the one document the most other
  readable documents in the scope point at, then its `spec` documents newest
  first — each document once, [AnchorCap] in all, every one readable by the
  reader. A pin the reader may not read takes no place. Anchors are left out of
  `recent` and do not count towards its cap. Nothing configures an anchor
  (docs/config.md); a person pins one with a gesture (`l2.RecordGesture`),
  which writes `l2.Store.Pin`'s record.
- **Only a line with an id.** An entity has a `line` only when it is a document
  the reader may read, and then it carries that document's `l1`.
- **Conflicts** come from a readable directive's references or text matching a readable current topic title or position. They are computed before budget trimming, capped at three, ordered by tier then topic id, and carry the current position, tier, and ratification stakes. Without a directive they are `[]`.

See [docs/design.md](../../docs/design.md#the-context-bundle).
