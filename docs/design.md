# Hearsay design doc

An open-source context platform for teams using coding agents. Hearsay ingests team communication and artifacts, distills them into layered knowledge with provenance, and serves scoped, permission-filtered context bundles to agents and humans through a small read/assert API.

Status: design draft. First consumer: `shed`. Generalized for any team.

## Problem

Decisions about a piece of work are made across Slack threads, meetings, issue comments, and PR reviews. When a human hands work to a coding agent, they either give it one of those sources or manually distill all of them into a long prompt. Hooking an agent up to Slack or Discord removes some friction but centralizes nothing. The session moves from a terminal to a chat thread and the context stays scattered.

Hearsay is the thing the agent asks instead. Chat apps, CLIs, and trackers become interfaces to it, not the place context lives.

## Non-goals

- Not an orchestrator. Hearsay does not assign, schedule, dispatch, or deliver work. Orchestrators (shed, others) consume Hearsay.
- No work-unit model. Issues, epics, and change units are entities that documents reference. Their shape and lifecycle belong to the tracker and the orchestrator.
- No message delivery. Directives are events like any other. Consumers subscribe to changes; nothing is pushed with delivery guarantees.
- No code mirror. Agents have the repository. Hearsay holds the vocabulary, ownership, constraints, and change history around code, not the code.
- No query-time synthesis. Hearsay does not answer questions with an LLM. It returns structured context; the consumer reasons.

## Layer model

|Layer|Holds|Answers|Mutability|
|---|---|---|---|
|L0|Raw events from every source|What happened|Append-only, tombstones for deletion|
|L1|Distilled documents, one per L0 artifact, flat rows in one schema|What was said|Rebuilt from L0|
|L2|Entities, topics, stances, and the edges between them|What it means|Incremental, human-correctable|
|L3|Derived views: current decisions, scope state, ownership|What is true right now|Rebuilt from L2 at any time, never written directly|

L0 and L1 are flat. L2 is the graph. LLM work happens at write time (L0 to L1, L1 to L2). Reads are structured lookups with no model in the loop.

## L0: events

Every source connector writes L0 only. An event is `{id, source, native_id, kind, time, payload (JSONB), acl}`. Sources include Slack threads and messages, GitHub issues, PRs, reviews and commits, meeting transcripts from Drive, wiki pages, tracker tickets, and agent session events (start, end, tool calls, assertions).

Agent activity is a first-class source. An agent's tool calls and proposals enter L0 so that later assertions can trace back to what the agent retrieved and chose.

The exact shape a connector emits — the id format, the `kind` vocabulary, the metadata `payload` must carry, how idempotency, edits and deletions work, and the interface a connector implements — is [the connector contract](connector-contract.md).

## L1: distilled documents

A common envelope plus a per-kind body. Every L1 doc is one row in one table.

```yaml
id: l1:slack:C123:1725812345.0001
kind: slack_thread | slack_burst | meeting_segment | pr | issue | commit | wiki_section | agent_turn
source: { system, native_id, url }
l0_refs: [event ids]                  # provenance, required
time: { created, updated, last_activity }
participants: [{ principal_id, role: author | reviewer | attendee | agent }]
scope: [entity ids]                   # tracker items, code entities, projects
references:                           # typed, extracted deterministically
  - { type: pr, id: dagger/dagger#1234 }
  - { type: system, id: engine }
  - { type: person, id: kyle }
  - { type: l1, id: l1:meeting:... }
acl: [...]                            # inherited from source at ingest, re-synced on change
text: ...                             # embedded
raw_text: ...                         # full-text index only, never embedded
embedding: ...

body:
  summary: ...
  question: ...                       # slack, issue
  outcome: ...                        # what was concluded, if anything
  outcome_kind: resolved | decided | proposed | open | none
  open_questions: [...]
```

Design points:

- `references` come from link parsing, @-mention resolution, and known system names, not from the LLM. They are the join key for L2 topic matching and cost nothing to recompute.
- `outcome_kind` is the L2 trigger. Only docs marked `decided`, `proposed`, or `resolved` enter the assertion pipeline.
- Meetings are segmented per topic. One meeting produces several L1 docs, each with one `outcome`.
- Long Slack threads also emit bursts (single-author runs that pass rarity and length gates) so tangent facts survive summarization.
- A secrets and PII scrub runs before `text` is written.
- The distiller is stateless and idempotent. Any L1 doc can be regenerated from its `l0_refs`.

## L2: the graph

### Entities

Anything documents are about. Entities form a hierarchy through `part_of` edges, and a document can reference entities at any level.

```yaml
entity:
  id: code:dagger/dagger:engine/server
  type: module | service | symbol | tracker_item | project | person | team
  aliases: [engine, the engine, engine server]
  path_patterns: [engine/server/**]     # code entities only; resolved against HEAD at read time
  part_of: [code:dagger/dagger:engine]
  owners: [principal ids]
```

Code is an entity namespace, not a document corpus. What the agent cannot get from the repo is the map from how humans talk ("the engine") to where the code is, who owns it, and what constraints apply. Path patterns rather than file lists survive refactors.

Tracker items (issues, epics, change units) are entities of type `tracker_item`. Hearsay knows their id and what references them. It does not know their state machine.

Aliases are learned by co-occurrence (a PR touching `engine/server/` linked from a thread that says "the engine" is one vote) and confirmed or rejected by humans. CODEOWNERS and repo structure seed the initial map.

### Topics and stances

A topic is a question the team has taken positions on. A stance is one position, from one source, at one time.

```yaml
topic:
  id: topic:...
  name: "migration strategy for engine schema v3"
  about: [code:...:engine/server, tracker:gh:dagger/dagger#1234]

stance:
  topic: topic:...
  position: "hand-run migrations for this release"
  author: principal id
  time: ...
  evidence: [l1 ids]
  supersedes: stance id | null
  tier: ratified | inferred | contested
```

The history of stances on a topic is the record. "Current decision" is a derived view (L3), not a stored fact. When a Slack thread decides one thing and a meeting later reverses it, both stances exist, the newer links to the older, and the meeting segment carries the reason.

Tiers:

- Ratified. A human confirmed it, or it landed in an authoritative artifact (merged PR, frozen spec, CODEOWNERS). Agents act on it.
- Inferred. The assertion pipeline's best reading. Agents cite it and proceed.
- Contested. Recent stances disagree. Agents ask before acting.

Source authority is configured per scope (see Configuration). A merged PR outranks a meeting, which outranks a Slack thread, which outranks a DM, unless a scope says otherwise.

### Anchors

Durable artifacts that define a scope, as distinct from recent activity. A design issue, an RFC, an ADR. Anchors are pinned explicitly, inferred as the most-referenced L1 doc within a scope, or defaulted by kind. Capped at four per scope; more than that is a signal the scope is too broad.

### L2 write path

Serialized per scope to avoid topic-merge races. For each new L1 doc with a qualifying `outcome_kind`, extract candidate assertions, match against existing topics using reference overlap first and embedding similarity second, then append a stance or open a topic. Topic merges and splits are cheap human actions. Wrong merges are the expected failure mode and must be one gesture to undo.

## L3: derived views

Rebuilt from L2 on demand. Never written directly.

- Current stances for an entity, including stances inherited from ancestor entities, tagged as inherited.
- Recent L1 activity on a scope.
- Open questions on a scope.
- Ownership and "who knows X" from participation and authorship.

## The context bundle

The primary read. Built deterministically from L2 and L3 for a given scope and principal. Pointers plus one line of distillation per pointer. Nothing raw. Every line carries an L1 or L0 id the consumer can follow.

```yaml
bundle:
  directive:                   # optional; the triggering event, verbatim
    text: "@agent skip the migration, we'll do it by hand"
    from: kyle
    via: slack:#eng-infra:thread-8891
    l0: evt:...

  scope:
    entities:
      - { id: tracker:gh:dagger/dagger#1234, line: "..." }
      - { id: code:engine/server, owners: [kyle], aliases_matched: ["the engine"] }

  anchors:                     # cap 4
    - { l1: l1:issue:1200, kind: design_issue, line: "engine schema v3 design", pinned_by: kyle }

  stances:                     # active only; inherited ones marked
    - topic: "migration strategy for engine schema v3"
      current: "hand-run migrations for this release"
      tier: inferred
      since: 2026-09-05
      supersedes: "automate via job"
      evidence: [l1:meeting:..., l1:slack:...]

  recent:                      # cap 5 by count, never by time
    last_activity: 23d
    items:
      - { l1: l1:pr:1250, kind: pr, when: 23d, line: "adds schema v3 table, merged" }

  open_questions:
    - { line: "who runs the migration in staging?", evidence: [l1:...] }

  conflicts:                   # computed, not retrieved
    - "directive matches inferred stance 'hand-run'; not yet ratified"

  handles: { get_l1, get_l0, search, stance_history, resolve }
```

Rules:

- Budget of roughly 1 to 2k tokens. Drop from the bottom. `recent` shrinks before `stances`. Ratified stances never drop.
- `recent` is capped by count. A scope idle for three weeks needs its last five touches more, not less. Age is surfaced as `last_activity`, never used as a filter. When trimming, dedupe by kind before cutting by recency so five CI comments do not crowd out the one meeting that changed the plan.
- `anchors` are not subject to the `recent` cap. The design doc is what the activity is about, not activity.
- No code content. Entity ids and path patterns only.
- Identical output regardless of which interface asked. That is the test of centralization.
- Cacheable. Same scope, same principal, no new events yields the same bundle.

## Read and assert API

Exposed over MCP and HTTP. Deliberately small.

|Call|Purpose|
|---|---|
|`get_bundle(scope, directive?)`|The primary read|
|`resolve(text)`|Aliases and mentions to entity ids|
|`stance_history(topic)`|Full record for a topic|
|`get_l1(id)`, `get_l0(id)`|Follow a pointer|
|`search(scope, query)`|Hybrid retrieval over L1 (embeddings, full text, rank fusion). Returns documents, not answers|
|`watch(scope, filter)`|Subscribe to new events on a scope. Notification only; consumers decide what to do|
|`assert(topic, position, evidence)`|Write a stance as an L0 event. Agents are principals|

Ratification, pinning, and topic merges are human actions taken in the tools people already use (see Human feedback loop).

## Access control

Four control points, from cheapest to most involved.

1. Ingest allowlist. Default deny at L0. Public channels, named repos, specified Drive folders, transcripts for tagged meetings. DMs and private channels are never ingested unless listed. What is not in L0 cannot leak.
2. ACL inheritance at L1. Every doc carries the source's access list at ingest and re-syncs on change.
3. Derived-object classification at L2. A stance inherits the most restrictive ACL of any of its evidence. Per-viewer recomputation is a later option if this proves too lossy.
4. Principal-based reads. Every read is filtered by an effective principal, the intersection of the agent's grants and the invoking human's. Handles fail closed.

Scopes filter for relevance. ACLs grant permission. They are never the same mechanism.

Agent classes:

|Class|Read|Write|
|---|---|---|
|observer|scoped L3 and L1|none|
|worker|scope plus linked code entities|`assert` (proposals)|
|orchestrator|multiple scopes|`assert`, subscribe|
|steward|all ingested|ratify, merge topics (human-only by default; the class exists so the door is there, closed)|

Every bundle served is itself an L0 audit event: who asked, on whose behalf, what scope, what was filtered.

## Deletion and provenance

L0 is append-only, but deletion is required (a pasted secret, a departed employee's data, a legal hold). Deletion writes a tombstone, then walks provenance forward. `l0_refs` on L1 and `evidence` on L2 give the full fan-out. Affected L1 docs are re-distilled without the deleted event; affected L2 objects are re-run. If a stance changes as a result, that is recorded as a supersession so nothing flips silently.

## Configuration

A repo, applied like GitOps.

- `sources/` connector configs: channels, repos, Drive folders, refresh cadence.
- `scopes/` named bundles of sources (Cerebras-style projects) and the tracker mapping for `tracker_item` entities.
- `principals/` identity mapping. `kyle` is Slack U123, GitHub kpenfound, kyle@..., and a calendar attendee. Without this, stances have no consistent author and authority cannot be checked.
- `code/` entity seeds: CODEOWNERS import, path patterns, initial aliases.
- `authority/` which principals and sources can produce ratified stances, per scope.

Adoption dies at the config step if identity mapping and connector onboarding are not near-zero effort. Defaults should cover a GitHub-plus-Slack team with a single file.

## Architecture and deployment

The implementation decisions behind this section — language, process model, storage,
LLM tiers, migrations, queue and observability — are recorded in [`docs/adr/`](adr/).

One Postgres with pgvector and full-text search holds all four layers. Vectors are an index on the L1 table, not a separate store. L2 is relational with recursive CTEs for hierarchy and supersession; it is small and shallow enough that a graph database is not warranted at the start, and the layering allows moving L2 alone later.

Four processes:

- Connectors. One per source. Push where the source supports it (Slack Socket Mode, GitHub webhooks, Drive change notifications), poll otherwise. Write L0 only.
- Distiller. L0 to L1. Stateless, parallel, retryable. Cheap model.
- Assertion worker. L1 to L2. Serialized per scope. Better model, low volume.
- API. Bundle assembly, MCP and HTTP endpoints, interface adapters, audit.

Self-hosted per organization. Containers, one compose file to start, Helm later.

Connectors are plugins. A third party ships a module that emits L0 events in the standard shape plus a source config entry, and its data is searchable and distillable with no special handling elsewhere.

## Human feedback loop

L2 will be wrong on a regular basis. Correction has to be one gesture in the tool the person is already in: an emoji reaction to ratify a stance, `/hearsay pin` on a thread, `/hearsay merge` on two topics, a reaction that demotes an inference. If correcting requires opening a separate UI, no one will.

## Evaluation

Log every bundle and what the consumer did next.

- Drill-down rate per bundle section shows what the bundle is missing.
- Conflict-flag precision shows whether stances are trustworthy.
- Time to ratification shows whether humans are engaging.
- Topic merge and split rate shows how often L2 is wrong.

Without these the distillation is just confident.

## Relationship to existing work

Agent memory layers (Mem0, Zep/Graphiti, Letta, Cognee, Neo4j Agent Memory) persist what an agent saw across sessions. They are agent-centric, do not ingest team communication, and have no stance or decision model. Enterprise knowledge systems (Cerebras Knowledge, Onyx, Glean) federate sources into search and stop at L1. Hearsay borrows Cerebras' one-table ingest, thread distillation, and hybrid retrieval for L1, and Neo4j's idea that agent reasoning is itself memory for L0. The stance history, provenance-driven deletion, and permission-filtered bundles are the parts nothing else does.

## Open questions

- Entity hierarchy rules. How `part_of` edges are built (repo structure, tracker hierarchy, manual) and who may restructure them.
- Authority config schema. What a per-scope authority declaration looks like and how it composes with identity mapping.