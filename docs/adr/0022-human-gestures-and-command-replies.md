# 22. Human gestures enter L0; only explicit commands receive source replies

- Status: accepted; the assertion worker does not interpret Discord slash commands, which the interaction adapter applies itself ([ADR-0024](0024-discord-commands-are-applied-by-the-interaction-adapter.md))
- Date: 2026-09-24

## Context

Discord reactions and commands, and GitHub `/hearsay` comments, let a person
correct L2 in the tool they already use (#24, #25). A connector's boundary is
L0: it describes source observations but has neither the identity mapping nor
authority to interpret them. Assertion and topic operations already serialize
on a scope's `assert` queue key ([ADR-0007](0007-postgres-backed-job-queue.md),
[ADR-0020](0020-topic-operations-are-a-ledger-held-on-the-scope-key.md)).
Discord also requires an interaction answer within three seconds, and merge
autocomplete needs an authorized L2 read. GitHub commands need a visible answer
without turning Hearsay into a bot that posts unsolicited content.

## Decision

**Gestures are L0 observations.** The Discord and GitHub connector event
shapers describe reactions, slash commands and `/hearsay` issue or PR comment
commands with the source event identity, actor identity, target and command
data. They ingest through the L0 gate; they do not interpret the gesture,
resolve a principal, read or write L1/L2, or send a source reply. A Discord
reaction removal or GitHub command-comment deletion is a tombstone for the
gesture. A GitHub command is recognized from the creation observation; an edit
does not execute a second command.

**The assertion worker interprets gestures.** Its L0 change-feed follower
enqueues a durable `assert` job with target `gesture:<source event id>` for each
gesture or withdrawal. It uses the gesture's scope as `serial_key`, the same
key as L1 assertion jobs and topic-operation holds. Repeated feed delivery
collapses to the same target; processing and the L2 gesture record commit
together so a retry does not apply the gesture twice. On restart the follower
replays missed changes. The worker resolves the source actor through configured
principal identities. An unmapped actor causes no L2 write. A mapped actor
must be a human allowed by that scope's `ratified_by.principals`; a failed
authority check also causes no L2 write. These refusals are durable outcomes,
not jobs retried until configuration changes. Authorized reactions act on the
original Discord message's distilled thread or burst, not a bot reply;
GitHub commands act on the issue or PR. Removing a reaction or deleting a
command reverses its gesture subject to the topic-operation undo rules.

**Only an explicit command may cause a source write.** Reactions receive no
reply. Discord interactions use an HTTP interaction adapter in the API runtime,
not the connector's Gateway loop: the adapter verifies the request with the
configured Discord application public key, records the command as L0, and
answers ephemerally with the interaction token within Discord's three-second
deadline. It may defer an ephemeral answer while the worker completes. The
adapter reads L2 for autocomplete under the invoker's mapped principal and
current access; `/hearsay merge` offers only topics that principal can read.
The configured Discord bot token is used to register commands, not to post
channel messages. The interaction token is used only for that command's
ephemeral response or follow-up.

For GitHub, the assertion worker posts exactly one reply comment for each
command, with the result or refusal and how to undo it, using the configured
GitHub source `secrets.token` (bot or app installation token with issue and PR
comment write permission). A durable reply record keyed by the command event
id and reconciliation against its source comment id prevent a retried job or
webhook replay from posting another reply. The connector never posts it.
Hearsay sends no other source writes and nothing unprompted.

**Commands and replies are control traffic, not team content.** The distiller
excludes Discord commands and GitHub `/hearsay` command comments, along with
Hearsay-authored replies, from L1 content. In particular, the GitHub reply's
webhook echo is recognized as Hearsay-authored and excluded; it cannot become
another command or feed a later distillation.

## Alternatives considered

- **Interpret gestures in connectors.** This gives source adapters identity,
  authority and L2 responsibilities and lets their writes race assertion jobs.
- **Handle Discord commands only through the Gateway and worker queue.** A
  queued job cannot promise the three-second interaction response or serve
  permission-filtered autocomplete at interaction time.
- **Post stance announcements for people to react to.** This adds unprompted
  channel traffic; the product decision is to react to the original message.

## Consequences

- Command ingress and response adapters need source credentials, but the
  connector's L0-only contract remains intact. Discord interaction ingress
  must reach the API runtime directly and share the same L0 ingest rules.
- The gesture job and reply record are implementation work for #179–#182;
  this ADR changes no runtime behavior. GitHub's configured token will need
  write permission for issue and PR comments.
- L0 retains command provenance while L1/L2 distillation treats it as control
  traffic. Refused gestures can be answered without being interpreted as team
  decisions.
