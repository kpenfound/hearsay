# 28. Readers traverse the L2 graph through bounded, typed steps

- Status: accepted
- Date: 2026-10-15

## Context

A bundle briefs a reader about one entity within a fixed budget. Questions
across entities — which features under a project are in flight, what decisions
concern them, who owns their code, or what a decision replaced — otherwise
need a call for each question. The graph already holds these relationships.
Readers need to follow them while keeping reach, evidence access and the
topic ledger authoritative.

An open-ended query language would enlarge the read surface and make work and
access checks harder to bound. Returning full documents or positions would
also duplicate the existing drill-down calls and spend context before a reader
has chosen what to read.

## Decision

**Readers traverse through bounded, typed steps, without a query language.**
`traverse` is one read call served by HTTP (`POST /v1/traverse`) and MCP. It
returns a subgraph of typed handles and one-line descriptive fields. Node
types are `entity`, `topic`, `stance`, L1 `document` and `principal`.

The edge vocabulary is fixed; each inverse swaps the source and target:

| Edge | Source → target | Inverse |
|---|---|---|
| `part_of` | entity → entity | `has_part` |
| `topics` | entity → topic | `about` |
| `documents` | entity → document | `scope` |
| `anchors` | entity → document | none |
| `owners` | entity → principal | none |
| `stances` | topic → stance | `topic` |
| `evidence` | stance → document | `cited_by` |
| `supersedes` | stance → stance | `superseded_by` |
| `author` | stance → principal | none |
| `participants` | document → principal | none |

Principals are terminal, reachable only from a visible node over a visible
edge. They cannot be starts and have no outgoing edges: finding an owner or
author does not permit enumerating everything that person wrote. L0 events
are not nodes. A reader follows a document's `l0_refs` through `get_l0`.

Requests have 1–10 starts of one type and 1–4 steps, each leaving the previous
step's target type. Depth defaults to `"1"`, accepts `"n"` or `"m..n"`, and
can exceed 1 only for `part_of`, `has_part`, `supersedes` and `superseded_by`,
up to 6. Filters apply only to the target type. `limit` counts non-start nodes,
defaults to 50, and accepts 1–200. The call stops with `truncated: true` on
reaching `limit` or examining 5,000 candidate rows, counted before visibility
filtering. A node appears once at its lowest step; starts are step 0. Edges
connect only returned nodes and use the stored direction, even for an inverse
walk. Candidate ordering is deterministic for the same graph and reader.

**Access is checked at every hop.** Entities require reach; documents require
`reader.MayRead`; topics and stances use `l2.Access` as `stance_history` does,
including withdrawals. An unknown start and an invisible start get identical
404 replies. A hidden node is neither returned nor traversed through, and
does not count toward `limit`. Hidden equals absent: below the row cap the
hidden-present and hidden-absent replies are byte-identical, including
`truncated`, except for the standing exception below. There are two exceptions,
recorded by owner decision:

- **Raw-row cap.** The 5,000-row cap counts rows before visibility filtering.
  When it trips, `truncated` and which later nodes are missing because the walk
  stopped early may depend on hidden rows. Bounded work takes precedence over
  strict hidden-equals-absent at this cap; counting only readable rows would
  allow arbitrarily many hidden candidates to be examined.
- **Standing follows `stance_history`.** A visible topic's `current`, `tier`
  and `tier` filter match, and its readable stances' `current` booleans, are
  computed exactly as `stance_history` computes them: `l2.Store.Assess` over
  the topic's whole history and its evidence, followed by `Access.Stance`.
  Hidden history or evidence may therefore change these values. If the current
  stance is hidden, the topic has no `current` or `tier`, never matches a
  `tier` filter, and none of its readable stances is `current` or matches
  `current: true`. A hidden earlier, non-current stance can also affect a
  contest: a readable current stance may be contested with that ancestor
  present and inferred with it absent. Changed `tier` or `current` filter
  matches can change which nodes are returned at that step and later steps,
  the edges between returned nodes, which nodes use up `limit`, and a
  limit-triggered `truncated`. These consequences belong to the same exception.
  Computing standing over readable stances only would disagree with
  `stance_history` and change the existing meaning of a topic's standing.

Under either exception, no node, edge or other reply field ever carries a
hidden node's id, name, position, author, evidence or content. Apart from the
differences these two exceptions allow, below-cap replies remain byte-identical
to hidden absence. Below the cap, hidden nodes that change no visible standing
retain that identity, `truncated` included.

Every topic read follows the topic ledger: merged-away topics resolve to their
merge target and split topics stand on their own. Tiers are computed through
`Assess`, never read from a row, and equal what `stance_history` serves the
same reader.

**Ids and lines only.** Descriptive text is cut to at most 200 bytes exactly as
`bundle.Line` cuts bundle lines. Replies include only the defined per-type
handle fields, never full positions, L1 text or L0 payloads. Readers continue
with `get_bundle` for an entity briefing, `stance_history` for a topic's full
record, or `get_l1` for a document. Traversal adds no search, caching or changes
to bundle sections or budgets.

## Alternatives considered

- **Cypher, GQL or another open-ended query form.** Adds an execution language
  and a broader access surface when a fixed edge vocabulary and typed steps
  answer the intended questions with bounded work.
- **A bespoke call for each question.** Repeats graph and access logic, and
  grows the API whenever a reader needs another combination of relationships.
- **Count only visible candidates toward the row cap.** Preserves stricter
  hidden-equals-absent behavior at the cost of unbounded scanning. The owner
  chose the raw-row bound and its explicit exception instead.
- **Compute standing from readable stances and evidence only.** Avoids hidden
  history affecting visible standing, but makes traversal contradict
  `stance_history`. The owner chose shared standing semantics and the explicit
  exception for those values and their consequences instead.
- **Return content during the walk.** Spends context on every reached node;
  existing drill-down calls let the reader choose which content to fetch.

## Consequences

- One call composes relationships across code, tracker and other entities
  while the bundle remains a fixed-budget briefing about one entity.
- Visibility must be checked before a node can become a frontier. The two
  exceptions limit hidden-equals-absent guarantees; neither permits hidden
  node data in any reply field.
- Stable ordering and explicit truncation make repeated walks predictable.
  A truncated subgraph can omit readable nodes because of either bound.
- New edge kinds must fit typed validation and access checks. Person-centric
  fan-out, L0 traversal and content retrieval remain outside this call.
