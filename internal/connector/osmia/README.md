# Osmia source

The `osmia` connector reads committed Osmia trace repositories through a read-only
mount. It does not import Osmia packages, open the writer lock, read working-tree
files, call models, or write to the source.

Configure an absolute `settings.root`, an owner user identity, a `channel` of
`owner` or `work`, and explicit project IDs as containers. Use distinct source
IDs for the two channels. Owner rulings and charter revisions belong to `owner`;
drafts, agent contributions, source commits and execution provenance belong to
`work`. Authorize automatic spec-artifact ratification only from the owner source.
Private role notes and memory watch records are excluded.

Backfill and poll pages pin Git commit, log file and line positions. Re-emission
uses stable record/revision IDs. A document's empty revision becomes a tombstone;
polling retracts current artifacts from removed containers. Bump
`permission_version` when restoring a previous ACL or re-adding a project.
The owner ACL is private by default and cannot be made public through this connector.

The sink, lifecycle, cursor and event rules are the
[connector contract](../../../docs/connector-contract.md). Tests use local Git
repositories and fake sinks, including interrupted pages and uncommitted edits.
