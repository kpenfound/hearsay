# 15. Runtime-owned streams for client-dialed sources

- Status: accepted
- Date: 2026-09-22

## Context

The connector runtime could call a poller or host a push handler. Discord's
Gateway instead requires the connector to dial and hold a WebSocket. Treating
that socket as a webhook misstates which side initiates and owns it. Polling a
connector-owned queue would make every Discord connector implement a second
supervisor, a bounded buffer, and durable sequence/session state across polls.

## Decision

Add the optional `Streamer` ingest mode. The runtime invokes `Stream(ctx,
sink)` in an owned goroutine and retries an ended call with capped backoff. The
runtime supplies the same Gate as every other ingest path and joins the stream
on shutdown. The connector owns its socket and protocol heartbeat, maintains a
session id and acknowledged dispatch sequence across reconnects, and resumes
before identifying anew. The sequence advances after the dispatch reaches the
Gate, so a failed sink write can be replayed. L0 ids make replays idempotent.
Permanent source rejections return `ErrStreamPermanent`, stop runtime retries,
and report failed health until an operator fixes the source and restarts it.

## Consequences

The base `Connector` interface is unchanged, but third-party implementers can
opt into a new mode. `Stream` is single caller per source. A process restart
loses its in-memory Gateway session; REST backfill in #89 covers that gap.
Until #89, visibility changes do not re-emit old Discord events under new ACLs.
