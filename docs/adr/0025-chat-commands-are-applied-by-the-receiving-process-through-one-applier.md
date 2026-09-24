# 25. Chat commands are applied by the receiving process through one applier

- Status: accepted
- Date: 2026-09-24
- Supersedes: the clause of [ADR-0024](0024-discord-commands-are-applied-by-the-interaction-adapter.md) that has the Discord interaction adapter apply a command itself

## Context

[ADR-0024](0024-discord-commands-are-applied-by-the-interaction-adapter.md)
has the API's Discord interaction adapter record a slash command, map the
Discord user to a configured human, read as that person and call
`l2.RecordGesture` or `l2.Operate`. All of that code lived in the adapter,
and its result was already the answer's words.

Slack slash commands (#212, part of #31) do not reach the API. With Socket
Mode, the connector's own socket receives them, in the connectors process,
and the envelope has to be acknowledged over that socket. A second copy of
the mapping, the view and the ledger calls in the connector would have to
make exactly the same decisions as the first, and a connector writes L0 only.
It cannot reach L2 at all.

## Decision

**A chat command is applied by the process that receives it, through one
shared applier.** That applier is `l2.Commands`. It takes the source, the
invoker's source identity, the verb (`pin` or `merge`), its arguments, and
the id of the L0 `command` event already recorded. It maps the invoker to a
configured human and reads as that person through `l2.View`, the view
`hearsay topics` reads through. A pin goes to `l2.RecordGesture`, keyed by
the command event, and a merge to `l2.Operate`. It returns a
`connector.CommandResult`, a value naming the outcome, never words.
The caller turns the result into its own ephemeral answer. Merge
autocomplete reads through the same view (`l2.Commands.MergeChoices`).

The API's Discord interaction adapter still verifies, records and answers a
Discord command. It applies it through the applier. A connector that receives
commands itself parses the source's request into a `connector.Command` and
hands it to its sink, which implements `connector.CommandSink`. The runtime
then records the `command` event through the ingest gate, calls the applier
the process wired in (`RuntimeOptions.Commands`; the connectors service builds
`l2.Commands` over its pool), and returns the result for the connector to
deliver. A source configured `read_only: true` gets `connector.ErrReadOnly`.
The runtime records nothing and applies nothing, and the connector answers
nothing.

The rules of [ADR-0022](0022-human-gestures-and-command-replies.md) and
[ADR-0023](0023-human-gestures-are-a-ledger-every-standing-reads.md) are
unchanged. The applier writes only through `RecordGesture` and `Operate`,
which hold the scope's `assert` serial key, so a command from the connectors
process serializes with the assertion worker's jobs and the API's commands
exactly as the API's did. A pin is keyed by its command event, so a command
delivered twice records one gesture. Both refuse a person the scope's
`ratified_by.principals` does not name. The distiller reads no document from
a `command` event, and the assertion worker still interprets only GitHub
commands from the feed.

## Alternatives considered

- **Each connector applies its own commands.** That means two copies of
  the mapping, the view and the refusals that have to agree, and a connector
  writing L2, which the contract forbids.
- **Forward the command to the API.** The connectors process would need an
  API credential and a route that is not one of the calls, and a command's
  answer would depend on two processes being up instead of one.
- **Enqueue a job and answer "received".** ADR-0024 already rejected this: the
  answer exists to state the result or the refusal.

## Consequences

- The connectors process now writes L2, but only through the applier, under
  the scope serial key, for commands a connector received.
- A command whose process dies mid-command is not retried. Nothing enqueued
  it. Its event may be in L0 with no gesture or operation. The person sees no
  answer and can run it again, and a pin run again is a new gesture.
- The words a person reads belong to the receiving surface. An outcome added
  to `connector.CommandResult` is one every caller has to phrase.
