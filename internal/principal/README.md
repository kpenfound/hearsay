# internal/principal

Identity: who said something, across every source they appear in.

**Belongs here:** the principal type, the per-source identity mapping from the
config repo's `principals/`, the resolver (resolved, ambiguous, unknown are all
outcomes callers must handle), and agent principals — an agent is a principal
too, and its class matters for access control.

**Does not belong here:** authority. Which principals may produce ratified
stances is per-scope policy and belongs with L2 and the config.

This package defines the types; `internal/config` parses `principals/` into
them, the way it parses `sources/` into `connector.SourceConfig`. It imports
`internal/connector`, for the identity hints an event carries, and nothing else
of Hearsay's — keep it that way, because everything above L0 imports this.

**Four things the code depends on and a reader would not guess:**

- **Matching is per source.** A handle in one source is no evidence about a
  handle in another. A person is listed once per source they appear in, and an
  email address matches an identity whose `handle` was written as one, in the
  same source.
- **Keys that disagree are ambiguous, not a precedence contest.** A native id
  survives a rename and a handle does not, so it is the key to *write down* —
  but when a hint's native id and handle name two different principals, the
  mapping has fallen behind the source, and resolving to either one would hide
  that for good. It is left unresolved instead.
- **Nothing unresolved is dropped, and nothing is minted for it.** An unknown or
  ambiguous identity is recorded for a person to map. L0 keeps the hint, so
  authorship comes back when the documents are distilled again; a placeholder
  principal id would leak into stances and outlive the gap it stood for.
- **A team never acts.** It owns things and stands in for a source's group. One
  of its members is who said something, so `AgentRead` and `HumanRead` refuse a
  team.

The mapping is read-only once `NewResolver` returns and the record of what did
not resolve is behind a mutex, so one resolver serves every connector at once.

Log principal ids, never email addresses or display names (ADR-0008). That
applies to an `Unresolved` too: it is made of source data.

See [docs/design.md](../../docs/design.md#access-control) and the
`principals/` section of [docs/config.md](../../docs/config.md).
