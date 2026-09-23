# 16. Entity hierarchy sources are ranked, and the highest replaces the rest

- Status: accepted
- Date: 2026-09-23

## Context

Entities form a hierarchy through `part_of` edges, and a view walks up it to
find the stances an entity inherits from its ancestors
([design](../design.md#l3-derived-views)). Until now only `code/` set those
edges by hand, and the loader refused a cycle in them. The design left open how
edges are built from repository structure and tracker hierarchy, and who may
restructure them afterwards ([ADR-0009](0009-configuration-as-a-gitops-directory.md)
touched it and left it). Feature #17 decided the answer; this records it.

Three sources can say what an entity is part of, and they can disagree:

- **Configuration**, `code/`'s `part_of`, which a person wrote.
- **Tracker hierarchy**: a sub-issue under its parent, GitHub sub-issues first.
- **Repository structure**: where a code entity's path patterns sit in its
  repository.

## Decision

- **The sources are ranked: `code/` over tracker hierarchy over repository
  structure.** For each entity, the highest-ranked source that sets any parent
  is the one its parents come from. They *replace* every lower source's parents
  and are never added to them: the same composition rule as `authority/`. A
  source that sets no parent for an entity replaces nothing.
- **Repository structure nests by path pattern, within one repository.** A code
  entity whose patterns all lie under another entity's is `part_of` the nearest
  such entity. A pattern lies under `dir/**` when its fixed directory prefix is
  `dir` or below it, and only a `dir/**` pattern contains anything. Two entities
  whose patterns cover each other's are not nested in each other. An entity that
  lies under nobody is part of its repository's project, `code:<owner>/<repo>`,
  where that entity exists. Patterns are never expanded into file lists, and
  nesting reads no repository.
- **A cycle in the merged graph drops the lower-ranked edge that closes it.**
  Edges are taken highest rank first, and within one rank in entity-id order; an
  edge that would close a cycle with those already taken is dropped and logged
  as a warning. A dropped edge is not replaced by a lower-ranked source's. The
  loader still refuses a cycle within `code/` itself.
- **People restructure, by editing `code/`.** Nothing inferred overrides what a
  person set. The feedback gestures in v0.8.0 will carry configuration's rank.

The merge is a pure function of the three sources' parents
(`l2.MergeHierarchy`, `l2.NestByPattern`), run when the assertion worker seeds
entities at startup.

## Alternatives considered

- **Union every source's parents.** An entity would sit under its configured
  parent and under whatever directory contains it, so a person moving a module
  in `code/` would not move it: the old parent would still claim it and pass
  its stances down. Replacement is what makes configuration mean something.
- **Rank each edge, not each entity's parent list.** A lower source could then
  add parents an entity's configured list left out, which is the union again
  under another name.
- **Refuse a merged graph with a cycle.** A tracker or a repository layout is
  not something an operator can fix by editing configuration, and refusing to
  start over it would make one odd sub-issue an outage. Dropping the inferred
  edge keeps what a person set and says so in the log.
- **Expand patterns against HEAD to compare file sets.** Exact, but it reads the
  whole tree at every seed and ties the hierarchy to files that move; the design
  keeps patterns so that a refactor does not invalidate the map.

## Consequences

- An entity nobody configured a parent for now inherits from the directory
  entity above it, so its bundle carries stances about that directory.
- A configured `part_of` is authoritative: adding one to an entity removes the
  parent its patterns implied.
- Tracker hierarchy has a place in the merge before it has a reader: #120 fills
  it from GitHub sub-issues.
- A pattern such as `**/*.go` or `engine/*/**` is nobody's parent, even where it
  covers another entity's paths; configure `part_of` for those.
