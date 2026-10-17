# 30. A work-updates source declares entities at runtime

- Status: accepted
- Date: 2026-10-16
- Supersedes: the three-hierarchy-sources clause of [Entity hierarchy sources
  are ranked, and the highest replaces the rest](0016-entity-hierarchy-sources-are-ranked-and-replace.md)

## Context

Factories and agents need to post projects, workstreams and updates without
configuration changes for each new artifact. These are ordinary information:
the existing document, stance and read APIs should serve them. Repository
reach alone cannot authorize discovery of a private project's name.

## Decision

A `work` push source accepts principal-token-authenticated posts. Projects and
workstreams emit `issue` roots; updates emit `message` children. State belongs
only on projects and workstreams. No project-specific read API is added.

The assertion worker declares runtime entities, at startup and while following
L0, with origin `source`, which seeded replacement preserves:

- `work:<source>:<project>`, type `project`, under existing `code:` entities for
  its declared repositories;
- `work:<source>:<project>#<workstream>`, type `tracker_item`, under its project.

Names follow the current artifact name, with no aliases. Missing repository
entities yield no edge and an info log containing ids only. No tracker mapping
is needed. Tombstones remove the entity and edges to it, leaving children.

Hierarchy inputs now rank `config > tracker > work > repo_structure`, in both
the full seed and incremental placement. The highest nonempty parent list
replaces lower lists and the existing cycle rule still applies. Only the
three-source list and rank in the earlier decision are superseded.

Principal grants accept syntactically valid `work:` ids before those entities
exist. Reach still limits relevance. A source-origin work entity is visible
only when the reader can read its current artifact document, using the full
audience including resolved containers grants. Missing documents, tombstones
and uncertain readability hide the entity. Resolve omits it, scoped reads
answer as for a missing entity, and related listings must not reveal it.
Repository access changes therefore affect entity discovery without a repost.

Projects defer access to every declared repository through
[An access list may defer to other sources' containers](0029-an-access-list-may-defer-to-other-sources-containers.md).
Without repositories, access is restricted to poster and author identities.
Children inherit the project's current list. A project post succeeds only
after propagating its list to all live children; identical reposts and startup
recover interrupted propagation, serialized against child posts.

The poster is the author by default. Only configured attributors may name a
configured human or agent as author; the update event then records the poster
as an agent participant. The document retains each principal’s strongest role,
so a poster that also authored its root remains an author there. Work identities resolve directly to principal ids without an
explicit identity mapping. Attribution sets the event's author and document
participants; authority is unchanged. The assertion worker assigns a stance's
author from the document's first author participant, as for every document.
Naming a human listed in `ratified_by.principals` does not ratify a stance:
that list authorizes hand ratification. A stance is ratified only through the
existing policy's artifact and source rules or a person's ratify gesture.

Generic `payload.touches` names repositories and optional paths. Deterministic
references add the existing repository project and matching configured code
entities to document scope. Work documents also include their own work entity
and their project's entity. When no scope covers the root's own container, use
the first scope by id covering a root touch, then `source:<id>`. Normal
distillation and assertion-worker recovery use that same root and key.

## Consequences

New projects and workstreams require no configuration deploy. Existing L1
distillation, authority, bundles and handles apply. Runtime entity discovery
now depends on current document access as well as reach, and fails closed
while ingestion or distillation catches up. The agent session source retains
its explicit identity mapping and semantic revision keys.

## Alternatives considered

A separate project model and read surface would duplicate documents and
hierarchy. Configuration-only entities would require a deploy per project.
Reach-only discovery would reveal private artifact names to readers granted a
repository scope but absent from its access list.
