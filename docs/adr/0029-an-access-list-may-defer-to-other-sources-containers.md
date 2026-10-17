# 29. An access list may defer to other sources' containers

- Status: accepted
- Date: 2026-10-16

## Context

Work updates describe projects across repositories. Their readers must be able
to read every repository the project declares, including after one goes private.
Copying repository permissions at ingest would become stale, and a connector
may read only its own source. A durable ACL re-sync can temporarily leave both
public and private revisions in a repository.

## Decision

An ACL entry may have kind `containers`, a `containers` array of
`{source, native_id}`, and a `native_id` key. Sort the list by source and native
id, deduplicate it, JSON-encode it, and take its SHA-256 hex digest as the key.
The contract's shared helper defines that encoding. Validation rejects a key
mismatch, an empty list, more than 16 containers, or an invalid source id.

At reader construction, resolve the containers entries present in L1 against
the reader's base public, identity and group audience. The reader must satisfy
every distinct access list on the current L0 revisions of every named
container. A container with no current revisions satisfies nobody. Access
lists retain their usual any-entry semantics internally; the container lists
are all-of. Resolution is one level, with no recursive deferred grants.

Add only `{kind: containers, native_id: <key>}` to the reader's audience for a
satisfied entry. Resolve anew for each reader construction. Ingest never
copies another source's access lists. `Reader.Allows` and the SQL ACL filter
continue to compare entries against the audience as before.

## Consequences

- Existing public, identity and group entries keep their meaning.
- A mixed public/private repository during re-sync fails closed for outsiders
  as soon as a current private revision appears. No work repost is needed.
- Missing repositories and incomplete ingestion deny access, even if another
  repository in the list is readable.
- Reader construction now queries current container access. The canonical key
  shares a resolved grant among documents with the same repository list.

## Alternatives considered

Copying ACLs into work events would require cross-source connector reads and
another propagation system for every repository permission change. Granting
access when any repository is readable would disclose projects spanning a
private repository. Both violate the required access boundary.
