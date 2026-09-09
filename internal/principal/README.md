# internal/principal

Identity: who said something, across every source they appear in.

**Belongs here:** the principal type, the per-source identity mapping from the
config repo's `principals/`, the resolver (resolved, ambiguous, unknown are all
outcomes callers must handle), and agent principals — an agent is a principal
too, and its class matters for access control.

**Does not belong here:** authority. Which principals may produce ratified
stances is per-scope policy and belongs with L2 and the config.

Log principal ids, never email addresses or display names (ADR-0008).

See [docs/design.md](../../docs/design.md#access-control).
