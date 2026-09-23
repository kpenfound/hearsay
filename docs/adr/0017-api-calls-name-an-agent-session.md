# 17. API calls name an agent session

- Status: accepted
- Date: 2026-09-23

## Context

ADR-0014 authenticates an agent acting for a human. The agent session source
records a session as revisions of one L0 artifact, but the API's bundle audits
and assertions have no link to that artifact.

## Decision

An agent may send `Hearsay-Session: <session id>` beside `Hearsay-Agent` on
either HTTP API surface. The id uses the agent connector's session artifact
grammar. The API checks that a configured agent source has a session start
event for the authenticated agent and human and that its ACL permits the read.
An absent header preserves the existing call and event bytes. A direct human
call naming a session is refused.

Bundle audit records and assertion records carry the session id and source id.
The source id disambiguates identical session ids across agent sources. A
session is part of an assertion's idempotency key, so assertions in different
sessions are distinct, while retries within one session remain idempotent.

`get_session(assertion)` follows the L0 assertion event id to that session's
ordered connector events and bundle audit events. It requires the assertion's
human and, on a delegated call, the same agent; the session start event must
still allow that caller. Its result is produced by the shared call layer, so
HTTP and MCP serve identical bytes. The audit record identifies a served
bundle by digest, as the existing audit contract does.

## Consequences

Agents must post a session start before making session-linked API calls.
Deployments that use this header must configure the agent source and map its
agent and human native ids to their principals. Session traces expose only
session events permitted by their ACL; their bundle audits are limited to
the authenticated session association.
