# The connector contract

What a source module must produce for Hearsay to ingest it, and the interface it
implements. This is the adoption boundary: a connector plus a `sources/` config
entry is the whole of what a third party ships, and everything above L0 — search,
distillation, the graph, bundles — then works on that source with no code that
knows it exists.

This document is normative and complete on its own. It is the specification for
`internal/connector`; where the two disagree, that is a bug in one of them.

Read [design.md](design.md#l0-events) for what the layers are, and
[design.md#access-control](design.md#access-control) for why ingest is default
deny.

## What a connector does, and what it must not

A connector turns one source into L0 events. That is all it does.

- It writes **L0 only**, through the sink it is given. It never writes L1, L2 or
  L3, never calls a model, and never decides what an event means.
- It **describes**, it does not **interpret**. It reports the identities a source
  gave it; it does not decide that a GitHub login and a Discord user are the same
  person (that is the identity mapping, issue #6).
- It reports the source's access list as it found it. It does not compute who may
  read anything.
- It emits everything it is allowed to and nothing it is not. What may be
  ingested is config, not a judgement the connector makes.

This includes Discord reactions and slash commands and GitHub `/hearsay`
issue or PR comment commands. Their source actor, target, event identity and
command data are described in L0. The assertion worker's L0 change-feed
follower interprets reactions and GitHub commands after mapping a human
principal and checking the scope's `ratified_by.principals`; the API's
Discord interaction adapter applies a slash command itself, with the same
checks, under the same serial key
([ADR-0024](adr/0024-discord-commands-are-applied-by-the-interaction-adapter.md)).
Unmapped or unauthorized gestures make no L2 change. The worker enqueues an
idempotent `assert` job targeted at
`gesture:<source event id>` under the scope's existing serial key, shared with
L1 assertion jobs and topic operations. The gesture record and L2 writes
commit together. A reaction removal or command-comment deletion reverses the
gesture under the same key. A GitHub command edit does not execute it again.
See [ADR-0022](adr/0022-human-gestures-and-command-replies.md).

The only source-write exception is an answer to a command the person issued.
Reactions receive no reply. The API runtime's HTTP Discord interaction adapter
verifies with the configured application public key, ingests the command as L0
and answers ephemerally with the interaction token within three seconds (or
defers an ephemeral answer and edits it in when the work completes); the
configured Discord bot token registers commands and is never used for channel
replies. That adapter,
not the connector, reads L2 for autocomplete under the invoker's mapped
principal, and merge choices are limited to topics the invoker can read. For
GitHub, the assertion worker uses the configured source bot/app `secrets.token`
with issue and PR comment write permission to post one result-or-refusal
reply per command. A durable record keyed by the command event and
source-comment reconciliation prevent duplicates on retry. Neither runtime
component sends any other source write or anything unprompted. Commands and
Hearsay-authored replies, including a GitHub reply's webhook echo, are L0
control traffic excluded from L1 distillation; the echo cannot become another
command.

A source configured `read_only: true` opts out of that exception
(`SourceConfig.ReadOnly`, [config](config.md#sources)). It is ingested exactly
as it would be otherwise, and its credentials need no write access, but nothing
reads its reactions as gestures, no command is registered or answered for it,
and nothing is posted to it. A connector that would describe a comment as a
`command` describes it as the ordinary content it then is, and does not declare
`command` in its descriptor. A connector that never writes, which is every
connector, starts and reports health on read-only credentials.

## The event

An event is one thing that happened at one source.

```json
{
  "id": "evt:github-acme:acme/api#12:comment:998",
  "source": "github-acme",
  "native_id": "acme/api#12:comment:998",
  "kind": "message",
  "time": "2026-09-09T12:00:00Z",
  "payload": { "...": "..." },
  "acl": [{ "kind": "public" }]
}
```

| Field | Type | Required | Meaning |
|---|---|---|---|
| `id` | string | derived | Hearsay's id, a pure function of `source` and `native_id`. A connector may omit it; ingest stamps it. |
| `source` | string | yes | The configured source instance the event came from. |
| `native_id` | string | yes | The source's identity for *this observation*. Ingest is idempotent on it. |
| `kind` | string | yes | What the event is: a core kind or a `<vendor>.<name>` extension. |
| `time` | RFC 3339 timestamp | yes | When the artifact happened **at the source** — not when Hearsay saw it, and not when this revision of it did. Every revision of an artifact carries the same value. |
| `payload` | object | yes | The standard metadata below, plus anything else under `native`. |
| `acl` | array | yes, non-empty | Who may read it, as the source described it. |

### Source ids

`source` is the id of a *configured source instance*, not a vendor: `github-acme`
and `github-oss` are two sources served by one connector type. It is the unit the
ingest allowlist works in, it appears in every event id, and it is not renamed
after ingest has started.

A source id is 1–64 bytes of lowercase letters, digits, `-` and `_`, starting with
a letter or a digit. `connector.ValidSourceID` is that rule, and it is exported
so that configuration rejects a bad id where it is written rather than when the
connector is built from it.

### Event ids

```
evt:<source>:<native_id>
```

with every byte of `native_id` outside `A–Z a–z 0–9 - . _ ~ : @ / # + = ,`
percent-encoded as `%XX` with uppercase hex — including `%` itself. Source ids
contain no colon, so the id splits on its first two and the native id comes back
byte for byte.

The id is **derived, not assigned**. Two consequences the rest of the contract
rests on: a connector that re-emits an observation produces the same id, and
anything holding an id (a bundle's provenance pointer, an L1 doc's `l0_refs`) can
recover the source and native id without a lookup.

A `native_id` is at most 512 bytes. An event id is longer than that, because
encoding turns one unsafe byte into three: the bound is
`len("evt:") + len(source) + 1 + 3 × 512`, at most **1605 bytes** for the longest
source id. Size the column from that number, not from the native id's limit — a
512-byte native id of non-ASCII text is legal and produces a 1551-byte id.

### Kinds

Every kind above L0 is written against the core vocabulary. Use a core kind
wherever the source has something that behaves like one.

| Kind | What it is | Author required | Title or text required |
|---|---|---|---|
| `message` | A chat message, or a comment on anything | yes | no — a message may be nothing but an attachment |
| `thread` | A container for messages | yes | yes |
| `reaction` | An emoji reaction; the human feedback loop reads these | yes | no |
| `issue` | A tracker item | yes | yes |
| `pull_request` | A change proposal | yes | yes |
| `review` | A verdict on a change proposal | yes | no — an approval may carry no body |
| `review_comment` | A comment anchored in a diff | yes | yes |
| `commit` | A commit on a watched branch | yes | yes |
| `document` | A wiki page, a design doc, a Drive file | yes | yes |
| `transcript` | A meeting transcript | no — the source may not name an owner | yes |
| `agent_session` | An agent session starting or ending | yes | no |
| `agent_turn` | One turn of an agent session | yes | yes |
| `tool_call` | A tool call an agent made | yes | no |
| `assertion` | A stance written through `assert` | yes | yes |
| `audit` | A bundle served or a session's handle call: who asked and ids served | yes | no |
| `tombstone` | An artifact deleted at the source | no | no |
| `deletion` | L0 events an operator deleted in Hearsay | yes | no |
| `command` | A command a person gave Hearsay in a source, such as a Discord slash command or a GitHub `/hearsay` comment; control traffic, never distilled | yes | no |

`assertion`, `audit` and `deletion` are written by Hearsay itself rather than by
a connector, under source `hearsay` (`connector.SelfSource`). They are in the
vocabulary because they are L0 events like any other. A `deletion` is not a
tombstone: it records that an operator redacted events with `hearsay delete
--apply` (ADR-0018), and it hides nothing by its own `target` — the redacted
rows carry the deletion that hid them.

A `command` is written under the source it was given in, by the runtime
component that received it: a Discord slash command by the API's interaction
adapter (ADR-0024), and a GitHub `/hearsay` comment, which arrives with the
comment webhook, by the GitHub connector. Hearsay's reply to a GitHub command
is `github.reply`, based on `command`. The distiller reads no document from
either, and leaves both out of the conversation they hang off (ADR-0022).

**Extension kinds.** A source with something genuinely different emits
`<vendor>.<name>` — two lowercase words separated by a dot, `figma.file_comment`,
`jira.sprint_change`. The vendor prefix keeps two connectors from meaning
different things by the same word. An extension kind **must** set
`payload.base_kind` to the core kind it behaves most like: that is what lets the
distiller handle a kind it has never heard of, and an extension kind without one
is rejected. The per-kind requirements in the table above are those of the base
kind.

Extending is the exception. If a source's artifact reads like a comment, it is a
`message`.

### Payload

`payload` is one JSONB column. Its standard fields are the same for every source,
so that the distiller works on a new connector's events with no code that knows
about it. Everything else the connector wants to keep goes in `native`.

| Field | Type | Required | Meaning |
|---|---|---|---|
| `artifact` | string | yes | The source's **stable** id for the thing this event is about, with no revision in it. Every revision of an artifact, and any tombstone for it, carries the same value. |
| `base_kind` | string | on extension kinds | The core kind an extension kind behaves like. Must be empty on a core kind. |
| `container` | object | yes | `{kind, native_id, name?}` — the repository, channel, direct message or folder the artifact lives in. |
| `url` | string | where one exists | The permalink a human would follow. |
| `title` | string | where one exists | The artifact's own title: an issue title, a document name, a thread name. |
| `text` | string | where there is any | What a human reads in the source, as plain text or markdown, with the source's markup left alone. This is the distiller's input. |
| `author` | identity | see the kind table | Who produced the artifact, as the source names them. |
| `participants` | array | where the source lists them | `[{identity, role}]`, role one of `author`, `reviewer`, `attendee`, `agent`. |
| `mentions` | array of identities | where the source resolves mentions | Identities the text refers to. |
| `links` | array of strings | where the source gives them | URLs the artifact carries, verbatim. L1 turns them into typed references; a connector does not. |
| `parent` | artifact id | where there is a parent | The thing this one hangs off: the issue a comment is on, the message a reply answers. |
| `thread` | artifact id | where there is a thread | The root of the conversation, which is what the distiller assembles a thread from. On a two-level source it equals `parent`. |
| `part_of` | artifact id | where the source has a hierarchy of items | The item this one is part of in the source's own hierarchy: a sub-issue's parent issue. It is not a conversation — nothing is assembled from it — and it may name an artifact in another container of the same source. An item with no parent leaves it out. |
| `paths` | array of strings | where the artifact is a change to files | The repository paths the change touches — a pull request's changed files, and the name a renamed file had — relative to the repository root. Sorted, without duplicates, at most 300 (`connector.MaxPaths`). Names only: never a diff, a patch or file content. |
| `paths_truncated` | boolean | when `paths` was cut | The change touched more paths than `paths` holds. |
| `revision` | object | exactly when `native_id` is `artifact@<token>` | `{token, edited_at?}`: `token` is that token, and `edited_at` is when *this revision* came about, which is what orders an artifact's revisions — see idempotency below. |
| `target` | artifact id | on tombstones only | The artifact the tombstone retracts. |
| `native` | any JSON | no | The source's own object, verbatim. L1 reads pull-request merge state here for its artifact class; nothing the fields above ask for may be hidden in it. |

`parent`, `thread`, `part_of` and `target` reference **artifact ids**, never
native ids: a comment hangs off an issue, not off one revision of it.

`part_of` is part of what an observation says, like any other field: an item
moved to another parent, or taken out of one, is a new revision, and the
current revision's `part_of` is where the item sits now. L2 reads it for the
tracker hierarchy ([ADR-0016](adr/0016-entity-hierarchy-sources-are-ranked-and-replace.md)).

`paths` is what L1 links a change to code entities by: the document references
every entity in `code/` whose path patterns match one of them, in the
repository the change is in. It is bounded so that a revision of a change that
touched ten thousand files is not ten thousand names in L0. A connector puts the
list in shape with `connector.BoundPaths`, which sorts it, drops duplicates and
anything validation refuses, and keeps the first 300 of that order, so what is
kept does not depend on the order the source listed the files in; a connector
that stops reading the source's list early sets `paths_truncated` too. Like
`part_of`, the list is part of what an observation says: a push that changes
what a pull request touches is a new revision with the new list.

Container kinds are `repository`, `channel`, `dm`, `folder` and `workspace`; a source
with a container of another sort may use another lowercase word. `workspace`
means the source itself, for a source with no smaller boundary. The container's
`native_id` must be the source's stable id — a repository's full name, a channel
id, a folder id — because it is what the ingest allowlist matches. Never a
display name.

### Identities

```json
{
  "source": "github-acme",
  "kind": "user",
  "native_id": "MDQ6VXNlcjE=",
  "handle": "kpenfound",
  "display_name": "Kyle Penfound",
  "email": "kyle@example.com"
}
```

`kind` is `user`, `bot` or `agent` — authority differs between them.

`native_id` must be the source's **stable** id: a Discord snowflake, a GitHub
node id, a Google account id. Never a login, a display name or an email: those
get renamed, and an identity mapping keyed on one breaks silently when they do.
`handle`, `display_name` and `email` are set where the source gives them, as
hints for whoever writes the mapping; they are never the key.

A connector emits identity **hints** and stops there. It does not mint Hearsay
principal ids, does not decide that two sources mean the same person, and does
not drop an event whose author it cannot place. The resolver (issue #6) maps
hints to principals and records the ones it cannot, so that a person can map them
later.

### ACL entries

`acl` is who may read the event, as the source described it at ingest. L1
inherits it, and L2 derives from it. It is **never empty**: an event nobody may
read cannot be read back, which is a bug rather than a private event, and ingest
rejects it.

| `kind` | Fields | Means |
|---|---|---|
| `public` | — | Every principal Hearsay knows may read it. |
| `group` | `source`, `native_id`, `label?` | The members of a source-native group: a channel's members, a repository's collaborators, a Google group. |
| `identity` | `source`, `native_id`, `label?` | One identity, for an artifact shared with a person rather than a group. |

`public` is scoped to the organisation running Hearsay, never to the internet: a
public GitHub repository is `public` because everyone in the organisation may
read it, not because anybody may.

Group membership is **not** resolved by the connector. The entry names the group;
access control resolves it at read time against the identity mapping. A connector
that expands a group into a list of identities has frozen a membership list that
will be wrong by next week.

Entries are additive: a principal matching any entry may read the event.

### Validation

Ingest rejects an event that breaks any of these, and a rejected event is a bug
in the connector rather than something to retry:

1. `source` is a well-formed source id.
2. `native_id` is non-empty, at most 512 bytes, and contains no whitespace or
   control characters.
3. `id`, if set, is the id derived from `source` and `native_id`.
4. `kind` is a core kind or a well-formed extension kind.
5. `time` is set.
6. `acl` is non-empty and every entry is well formed.
7. An extension kind carries a `base_kind` that is a core kind; a core kind
   carries none.
8. `payload.artifact` is non-empty, and `native_id` is either `artifact` or
   `artifact@<revision>`.
9. `payload.container.kind` and `payload.container.native_id` are non-empty.
10. The kind's author and content requirements are met.
11. Every identity has a well-formed source, a non-empty `native_id` and a known
    kind; every participant has a known role.
12. A tombstone has a `target`, and it is not the tombstone's own `artifact`;
    nothing else has a `target`.
13. `payload.revision` is present exactly when `native_id` is
    `artifact@<token>`, `token` is non-empty, and `payload.revision.token`
    equals it.
14. `payload.revision.edited_at`, where set, is not earlier than `time`.
15. `payload.part_of`, where set, is not the event's own `artifact`.
16. `payload.paths` holds at most 300 paths, each non-empty, not starting with
    `/` and with no control character, sorted in byte order without duplicates;
    `payload.paths_truncated` is set only alongside a non-empty `paths`.

## Idempotency, edits and deletions

Three ids, doing three different jobs:

- **artifact** — the thing. Stable for its whole life, across every edit and its
  deletion. `acme/api#12`, a Discord message id, a Drive file id.
- **native_id** — one *observation* of the thing. Equal to the artifact id for an
  artifact that cannot change, and `<artifact>@<revision token>` for one that
  can.
- **id** — Hearsay's id, derived from the source and the native id.

**Re-emitting is free, and is the expected behaviour.** Ingest is idempotent on
the event id: an event whose id is already in L0 writes nothing, and one an
operator deleted stays deleted (see deletions below). A connector that
is unsure whether it already sent something re-sends it. That is what makes a
webhook overlapping a backfill safe, and it is why a connector is not required to
remember what it has emitted.

The rule that makes it work: **two emissions that could differ in `payload` or
in `acl` must have different native ids.** If a source can change an artifact —
its content or who may read it — the connector puts the source's own version
token in the native id.

**An edit is a new event**, never an overwrite: L0 is append-only. The new event
carries the same `payload.artifact`, a `native_id` of `<artifact>@<token>` and a
`payload.revision` naming the token and, where the source says, when the edit
happened. Every earlier revision stays in L0 — that is the provenance for
anything distilled from it — and the newest revision of an artifact is the
current one.

### Which revision is the current one

`time` is when the artifact happened at the source and does not move: a message
posted at 10:00 that is edited at noon, or has its permissions changed a month
later, still has `time` 10:00 on every revision. That is what makes `time` usable
for recency and for a thread's ordering — and it is why `time` is **not** what
orders revisions. Every revision of an artifact is tied on it by construction.

Revisions of one artifact are ordered by:

1. `payload.revision.edited_at`, latest first, where both have one.
2. Ingest order — the order L0 received them — otherwise.

A connector's part in that is one sentence: **put the source's time for *this
revision* in `payload.revision.edited_at`, and leave `time` alone.** An edit
timestamp, a Drive revision's `modifiedTime`, the time a permission change was
observed. Where the source gives none, omit it and ingest order decides, which is
correct because a re-emission is by definition later than what it revises.

`edited_at` is never earlier than `time` — a revision cannot precede the thing it
revises — and validation rejects an event where it is, because that is the
symptom of a connector putting the artifact's own time in the revision's field,
which is exactly the mistake that would order an edit before the original.

The revision token is whatever the source gives that changes when the
observation does: an edit timestamp, an ETag, a Drive revision id, a GitHub
`updated_at`. It is opaque to Hearsay. A connector whose source has no single
token for that — content in one version field, permissions in another — composes
one, `<content token>+<permission token>`, and puts it in `payload.revision`
verbatim. Where an artifact has no content token at all — something the source
treats as immutable — the token is the permission part alone.

### When only the access list changes

An artifact whose ACL changed and whose content did not is a **new revision**,
not an exception. Discord channel `C123` is public, so every message in it
carries `acl: [{public}]`; someone makes the channel private. Nothing about any
message changed, so no `edited_timestamp` changed, so re-emitting with the same
native ids would deduplicate to nothing and L0 would keep `public` on every one
of them forever — which is what design.md's "re-syncs on change" exists to
prevent.

So: the connector re-emits each affected artifact with a native id whose
revision token reflects the new permissions (`<message id>@perm:<version>`), the
same payload, the same `time` — the message was still posted when it was posted —
`payload.revision.edited_at` set to when the permission change happened where the
source says so, and the new `acl`. The newest revision of an artifact is the
current one by the order above, so the ACL re-syncs by the same rule edits do,
with no second mechanism and nothing rewritten in place.

An ACL re-sync is the case that makes the ordering rule load-bearing: the
original and the re-emission are tied on `time`, and resolving that tie towards
the original would keep `public` on a message in a channel that is now private.

Two consequences worth stating plainly, because they are the cost of that
choice:

- **It is a bulk operation.** A container that changes visibility means
  re-emitting every artifact in it. That is what a re-sync is, and the runtime
  drives it the way it drives a backfill: a connector that implements
  `Resyncer` does bounded work per `Resync` call, and the runtime stores the
  cursor after each one, so a re-sync interrupted by a restart resumes.
  Re-emitting an artifact whose permissions have not changed is deduplicated
  away, so walking part of a container twice costs nothing.
- **What is owed is durable, and so is how it is found.** A push connector told
  that a container stopped being public records the re-sync through its sink
  (`ResyncRequester`) before it answers the delivery, so an answered delivery is
  never a lost one. A delivery that never reached a running process, or failed,
  is caught at startup: the runtime asks the connector (`Public`) about every
  container L0 still serves as public and owes a re-sync for each the source
  says is not. A container re-synced since its newest public artifact arrived is
  not asked about, because what that walk could not reach a second walk would
  not reach either ([ADR-0013](adr/0013-acl-re-syncs-are-durable-and-driven-by-the-runtime.md)).
- **Until the re-emission lands, L0 holds the old ACL.** Ingest is eventually
  consistent with the source's permissions, and it is more permissive than the
  source in the window between the change and the re-sync. A source whose
  permissions must be enforced at read time in real time is not served by ACL
  inheritance at all; that is a read-path decision and belongs to access control
  (issue #21), not to a connector.

**A deletion at the source is a tombstone event**: a new event of kind
`tombstone` whose `payload.target` is the artifact id it retracts, timed when the
deletion happened. The connector does not re-emit the artifact and does not
delete anything. The tombstone is a fact about the source, and the forward
provenance walk that re-distills what depended on the artifact is Hearsay's
(issue #23).

A tombstone is **its own artifact**, not a revision of what it retracts:
`payload.artifact` is `<target>:tombstone` by convention, and validation rejects
a tombstone whose artifact is its target. That is what makes a deletion notice
delivered twice land on one id — a tombstone timed by when the delete arrived
would produce a second event on every redelivery.

That is deletion **at the source**. Deletion **from Hearsay** — a pasted secret,
a departed employee, a legal hold — is an operator action that redacts payloads
and walks provenance forward. A connector has nothing to do with it, except
that its replays of a redacted event are dropped: re-emitting an event an
operator deleted stores nothing, returns no error and is counted on the
deletion record, so a backfill or a redelivery neither restores the content nor
stalls the connector. A new revision of the artifact — a new event id — is
admitted, the same as after a tombstone
([ADR-0018](adr/0018-operator-deletion-redacts-l0-in-place.md)). The L1
documents built from the deleted events are re-distilled or deleted, the
stances resting on them are superseded, and the text L2 read from the deleted
content is redacted once it is no longer current; a tombstone gets the same
rebuild without that redaction
([ADR-0019](adr/0019-operator-deletion-redacts-superseded-l2-text.md)).
`hearsay delete list` and `hearsay delete show <id>` are the record of each
deletion and of what it rebuilt.

A tombstone carries the same ACL as the artifact it retracts, so that the
retraction is visible to exactly the people the artifact was.

L0 hides revisions ingested before a tombstone for their artifact. A later
eligible revision with the same stable artifact id becomes current; the older
revisions remain retracted history and the tombstone remains in the change
feed. Replaying an existing tombstone keeps its original event id and ingest
position, so it cannot hide that later revision. A later retraction requires a
new tombstone event id. Event ids still reject changed payloads on replay.

## The ingest allowlist

Default deny at L0 (design, access control, control point 1). What is not in L0
cannot leak, so this is the cheapest control point and the one that must not be
bypassable by a connector.

The allowlist is not a separate list: it is the `containers` of the configured
sources. An event is written only if its `source` is configured **and** its
`payload.container.native_id` is one of that source's containers. A source's
single container entry `*` widens it to every container the credentials can see,
which is a deliberate choice a person makes in config, per source — the fallback
for a source whose whole boundary is the source itself, such as an agent's own
session stream.

Enforcement is in the runtime, not the connector: every event goes through a gate
that checks the source, validates the event, checks the kind against what the
connector declared, and only then writes. A connector cannot opt out of it, and a
connector that emits from a container config does not name is not broken — the
event is dropped and counted, and ingest carries on. The three checks before it
are contract violations and are returned to the connector as errors.

Emitting for a source other than the one the connector was configured for is
always an error, never a drop.

Filesystem reconciliation can retract a document after its old folder leaves
the allowlist. The runtime gate offers `RetractionSink.Retract` for a validated
`tombstone` in this case; ordinary `Emit` keeps the allowlist check. The
poller obtains the prior artifact and ACL from the durable
`DocumentInventory.Documents(ctx, source)` read on its sink. This read returns
current documents regardless of the current folder allowlist, including
documents that must now be retracted. A sink without that read cannot safely
perform removal reconciliation.

## The Go interface

A connector is a Go module that exports a `Factory`. Whoever builds the binary
registers it; there is no global registry and no `init`-time registration, so
what a binary can ingest is readable from its wiring.

```go
type Factory func(ctx context.Context, src SourceConfig) (Connector, error)

type Connector interface {
    Describe() Descriptor
    Health(ctx context.Context) Health
    Close(ctx context.Context) error
}
```

A `Connector` on its own cannot produce anything. Every connector implements at
least one of the three ingest modes, and the runtime refuses to start one that
implements none:

```go
// Push: the source calls us.
type Pusher interface {
    Connector
    Handler(sink Sink) http.Handler
}

// Poll: we call the source, on the source config's refresh cadence.
type Poller interface {
    Connector
    Poll(ctx context.Context, sink Sink) error
}

// Optional: a live change feed whose position must survive a restart.
type CursorPoller interface {
    Poller
    PollFrom(ctx context.Context, sink Sink, from Cursor) (Cursor, error)
}

// Optional sink read for a feed that reports deletion without old ACL data.
type ArtifactReader interface {
    CurrentArtifact(ctx context.Context, source, artifact string) (Event, bool, error)
    CurrentArtifacts(ctx context.Context, source string) ([]Event, error)
}

// Optional sink wake for a verified notification.
type PollRequester interface { RequestPoll() }

// Stream: we dial the source, which sends events on the connection.
type Streamer interface {
    Connector
    Stream(ctx context.Context, sink Sink) error
}

// Optional: history.
type Backfiller interface {
    Connector
    Backfill(ctx context.Context, sink Sink, from Cursor) (BackfillResult, error)
}

// Optional: a completed history walk invalidated by config changes.
type BackfillVersioner interface {
    Backfiller
    BackfillVersion() string
}

// Optional: containers that can stop being public.
type Resyncer interface {
    Connector
    Public(ctx context.Context, container string) (bool, error)
    Resync(ctx context.Context, sink Sink, container string, from Cursor) (BackfillResult, error)
}
```

Push where the source supports it, poll for bounded reads, and stream for a
client-dialed live connection. A connector may implement several modes, and a
live source still needs `Backfiller` to get its history.

- **`BackfillVersion`** identifies the configured input set for a completed
  backfill. The runtime stores its short, printable value with the done cursor
  and starts a new bounded walk on a later process start if it changes. A
  connector changing its input set while a walk is unfinished also checks the
  version in its own cursor before resuming.
- **`Poll`** is never called concurrently with itself, so a poller may keep its
  position in memory without locking. It returns when it has emitted what one
  pass found. An error is retried on the next tick with backoff.
- **`PollFrom`** takes a separate durable cursor when a connector implements
  `CursorPoller`; the runtime saves its successor only after the call succeeds.
  A failed call replays from the former cursor. Its `Poll` method is not called
  by that runtime. A verified push notification may request an early poll;
  ordinary cadence remains the fallback.
- **`Stream`** runs in a runtime-owned goroutine until its connection ends or
  its context is cancelled. The runtime retries an ended stream with the same
  capped backoff as a failed poll. The connector owns its socket, heartbeat,
  source session and replay sequence; it advances the sequence only after
  emitting the dispatch. Its `Close` stops and joins any goroutines it starts.
  An unspecified `refresh` starts retries at the runtime's minimum refresh
  (30 seconds), rather than a poller's five-minute default.
  A stream that receives a permanent source rejection returns
  `ErrStreamPermanent` and reports failed health; the runtime stops retrying
  it until the process is restarted after an operator fixes the source.
- **`Handler`** is mounted by the runtime under a path it owns — `/hooks/<source
  id>` in Hearsay's own runtime, which is the URL the source is configured to
  deliver to. The handler verifies the source's own signature over the request —
  the runtime cannot, the scheme is the source's — and a delivery it cannot
  verify is rejected with nothing emitted.
- **`Backfill`** does a bounded amount of work per call — a page, a day, whatever
  the source's API pages by — and returns `{Next, Done, Events}`. The runtime
  stores `Next` and hands it back, so a backfill interrupted by a restart
  resumes. The first call gets the zero cursor. A `Cursor` is an opaque string
  that must survive a restart, so it may not refer to anything held in memory,
  and it is *stored* as text: valid UTF-8, no NUL byte, at most 4096 bytes.
  Arbitrary bytes go in as base64 rather than as themselves.
- **`Resync`** re-emits one bounded piece of one container's artifacts under the
  ACL they have now, and means what `Backfill` means, for that container. The
  runtime keeps one record per container — owed or not, the cursor, a count of
  requests, when the last one finished — and a request that arrives during a
  walk starts it again. A connector asks for one by type-asserting its sink to
  `ResyncRequester` and calling `RequestResync(ctx, container)`, which returns
  once the record is written and fails when the runtime has nowhere to keep it.
  **`Public`** is a network call the runtime makes at startup, never on the
  health path.
- **`Health`** returns `ok`, `degraded` or `failed` with a detail line and the
  time of the last event. It must not make a network call, and its detail carries
  no credentials, no event text and no personal data: health is served more
  widely than L0.
- **`Close`** is called once and returns when every goroutine the connector
  started has stopped. A connector owns no goroutine that outlives it.

`Descriptor` is `{Type, Kinds}`: the type the config selects it by, which must
match the type it was registered under, and every kind it can emit, including its
extension kinds. `Kinds` is enforced — emitting an undeclared kind is an error —
so that what a source can produce is readable from config rather than discovered
in production.

`Sink` is where events go:

```go
type Sink interface {
    Emit(ctx context.Context, ev Event) error
}
```

`Emit` is safe for concurrent use and safe to call with an event that was emitted
before.

### The source config a connector consumes

One entry of the config repository's `sources/` directory, parsed. The on-disk
format is [the configuration schema](config.md#sources); this is what reaches the
connector:

| Field | Meaning |
|---|---|
| `ID` | The source id. Goes in every event. |
| `Type` | The connector type, which selects the factory. |
| `Containers` | The repositories, channels or folders this source may ingest, by native id. Default deny; `*` widens it. |
| `Refresh` | How often the runtime calls `Poll`, and the retry base for `Stream`. Ignored by a push-only connector; the runtime applies its own floor and jitter. A stream with none retries from the minimum refresh (30 seconds). |
| `ReadOnly` | Hearsay never writes to the source ([above](#what-a-connector-does-and-what-it-must-not)). A connector that would describe a comment as a `command` describes it as ordinary content instead, and declares no `command`. |
| `Settings` | The connector's own configuration, as JSON. Decode it with `DecodeSettings`, which rejects unknown fields so that a typo in config fails at startup. |
| `Secrets` | Credentials, resolved by the runtime from the environment. They never live in the config repository, which is checked in: config names a secret, the runtime supplies its value. |

A factory validates its settings and returns an error rather than a connector
that will fail later. A source that cannot start is a startup failure, not a
health status.

### Testing a connector

`internal/connector` ships a `Fake` connector implementing all three modes and a
`Recorder` sink. A connector's own tests should drive it through a `Gate` built
from the same `SourceConfig` as the allowlist, because that is what runs in
production, and assert on what the recorder received.

## Connector examples

Written before the connectors, and checked on paper against the GitHub (v0.2.0),
Discord (v0.3.0), Drive (v0.4.0) and Obsidian (v0.4.0) work.

### GitHub (issue #8)

| Artifact | kind | artifact id | native_id | container |
|---|---|---|---|---|
| Issue | `issue` | `acme/api#12` | `acme/api#12@<updated_at>`, or `…@<updated_at>+parent:<parent artifact>` for a sub-issue | repository `acme/api` |
| Issue or PR comment | `message` | `acme/api#12:comment:998` | `…@<updated_at>` | repository |
| `/hearsay` command comment | `command` | `acme/api#12:comment:998` | `…@<updated_at>` | repository |
| Hearsay's reply to a command | `github.reply`, base `command` | `acme/api#12:comment:999` | `…@<updated_at>` | repository |
| Pull request | `pull_request` | `acme/api#31` | `…@<updated_at>`, or `…@<updated_at>+paths:<hash of its paths>` where it touches any | repository |
| Review | `review` | `acme/api#31:review:77` | `…@<hash of state and body>` ([ADR-0012](adr/0012-a-github-review-is-versioned-by-a-hash-of-its-content.md)) | repository |
| Review comment | `review_comment` | `acme/api#31:comment:88` | `…@<updated_at>` | repository |
| Commit on the default branch | `commit` | `acme/api@<sha>` | same — a commit's content does not change | repository |

`parent` is the issue or pull request for a comment or review, and `thread` is
the same: GitHub conversations are two levels. Author is the GitHub node id with
the login as `handle`. ACL is `public` for a public repository and
`{group, native_id: "acme/api"}` for a private one, which is the collaborator set
resolved at read time rather than a frozen list. Webhooks are `Pusher` with
signature verification in the handler; backfill is `Backfiller` over the REST
list endpoints with the page cursor; `repository.privatized` is a
`RequestResync`, and the re-sync is `Resyncer` over the same walk for one
repository, with no start date. Deleting a comment sends
`issue_comment.deleted`, which is a `tombstone` with artifact
`acme/api#12:comment:998:tombstone` and `target` the comment's artifact id.

An issue or pull request comment whose first line starts with the word
`/hearsay` is a `command`, with the same artifact, parent and thread as any
comment. Its `native` is the command read from it —
`{"id": 998, "repository": "acme/api", "issue": 12, "command": "merge", "args": ["topic:…", "topic:…"]}` —
and `"edited": true` on a revision whose `updated_at` is after its
`created_at`, which Hearsay does not run. A comment carrying the invisible
line `<!-- hearsay:reply comment=<id> -->`, which every reply Hearsay posts to
a command ends with, is a `github.reply` naming that command in
`native.reply_to`, whatever its text says, so a reply's webhook echo is never
a command. Deleting either is the same tombstone as for any comment.

A sub-issue is `part_of` its parent issue, `acme/api#10` or an issue in another
repository, read from the `parent_issue_url` the REST lists and the webhooks
both carry. A `sub_issues` delivery (`parent_issue_added`, `parent_issue_removed`
and their `sub_issue_*` mirrors) emits the sub-issue again, with the parent it
was added to or with none; the delivery about a sub-issue in another repository
is left to that repository's. GitHub does not promise to move `updated_at` when
only the parent changes, so a sub-issue's content token names its parent as
well — `2026-09-09T12:00:00Z+parent:acme/api#10` — and moving it, or taking it
out, is a new revision rather than a native id that comes back with different
content.

A pull request carries the paths it touches in `paths`, read from its files list
(`GET /repos/{repo}/pulls/{n}/files`), which gives each file's name and, for a
rename, the name it had — and a patch, which the connector never decodes. A
`pull_request` delivery carries no file list, so every delivery reads it again:
a push to the branch (`synchronize`) moves `updated_at` and changes the paths
together. The read stops once it holds 300 paths, three pages at GitHub's
largest page size, and sets `paths_truncated` where GitHub had more. The files
list is read separately from the pull request, so its content token names the
paths as well: `2026-09-09T12:00:00Z+paths:` and the first 16 hex digits of the
SHA-256 of every path followed by a newline, then `truncated` where the list was
cut. A backfill and a delivery that read the same list emit the same event.

Every native id with an `@` carries `payload.revision.token` equal to the part
after it, so an issue at `acme/api#12@2026-09-09T12:00:00Z` has that timestamp as
its token. A repository going private is an ACL change with no content change,
and GitHub gives no version for visibility, so the connector composes the token
the way the general rule says: `<content token>+perm:private` where the artifact
has a content token, and `perm:private` alone where it does not. Commits are the
second case — a commit's content never changes — so `acme/api@<sha>` re-emits as
`acme/api@<sha>@perm:private`. Without that clause that row would re-emit with
its original native id, deduplicate to nothing, and keep `public` on every commit
in a repository that is no longer public.

A review is the first case, though GitHub gives it no version: its body can be
edited and it can be dismissed, and no field moves when either happens. Its
content token is therefore the first 16 hex digits of the SHA-256 of its
lower-case state, a newline, and its body
([ADR-0012](adr/0012-a-github-review-is-versioned-by-a-hash-of-its-content.md)),
so an edited or dismissed review is a new revision and a backfill that reads it
after the fact does not collide with what L0 already holds. The source gives no
time for the change, so the revision has no `edited_at` and ingest order decides.

Checks out. The one thing worth naming: an edit to an issue body arrives with the
same `updated_at` granularity as a label change, so a connector emitting on every
`issues` webhook produces revisions whose content is identical. That is
acceptable — a revision that changes nothing is a row in L0, and the distiller
regenerates from the latest — but a connector that wants to avoid it hashes the
content into the revision token instead of using `updated_at`.

### Discord (issue #13)

| Artifact | kind | artifact id | native_id | container |
|---|---|---|---|---|
| Message | `message` | `<message id>` | `<id>@perm:<hash>` or `<id>@<edited_timestamp>+perm:<hash>` when edited | channel `<channel id>` |
| Thread | `thread` | `thread:<thread id>` | `<artifact>@perm:<hash>` | channel `<parent channel id>` |
| Reaction | `reaction` | `<message id>:reaction:<user id>:<emoji>` | same | channel |
| Deleted message | `tombstone` | `<message id>:tombstone` | same, with `target` `<message id>` | channel |
| Deleted thread | `tombstone` | `thread:<thread id>:tombstone` | same, with `target` `thread:<thread id>` | parent channel |
| Removed reaction | `tombstone` | `<reaction artifact>:tombstone` | same, with `target` `<reaction artifact>` | channel |

Discord gives `edited_timestamp` as `null` until a message is edited. The
permission component is always present, including for a public channel with
no overwrites, so a later public→private→public transition does not try to
re-use the original public event id. It is the first 16 hex digits of SHA-256
of the parent channel's permission overwrites sorted by id and type, with an
empty list encoded as `[]`. Discord provides no permission-change timestamp,
so those revisions have no `edited_at`; an edited message keeps its source edit
timestamp in `edited_at`. Threads are their own channel
ids, so a message in a thread has `container` = the parent channel (what the
allowlist names) and `thread` = the thread's artifact id — the distinction the
contract draws between container and thread is load-bearing here. Reactions carry
their author and no text, which is why `reaction` requires neither. ACL for an
allowlisted private channel is `{group, native_id: <channel id>}`, so the member
set is resolved at read time.
The `thread:` artifact prefix is necessary because a public thread and the
message it was started from share the same Discord snowflake. The message keeps
the bare snowflake; both artifacts can then coexist in L0.
A message directly in a thread has `parent` set to the thread artifact. A
message replying to another message has that message as `parent`; both keep the
same `thread` inside a native thread. A reply in an ordinary channel has only
`parent` and joins that channel's fixed time window.
The reaction emoji component is the custom emoji snowflake when one exists,
otherwise the Unicode emoji URL-escaped so punctuation cannot change the id's
structure.

Discord is a `Streamer`: it dials the Gateway and resumes a session after an
interrupted connection. The runtime owns retry, while the connector owns
IDENTIFY, heartbeats, sequence and RESUME. A private thread uses its own group
id so its narrower membership is not widened to the parent channel. Gateway
delete and reaction dispatches omit an occurrence timestamp; their `time` uses
the target message snowflake's source creation time.

Discord is also a `Backfiller` and `Resyncer`. It walks parent messages and
active and archived threads through REST using a stored page cursor. A fresh
Gateway READY requests a durable walk for missed messages after an outage;
`CHANNEL_UPDATE` requests one when the channel's public visibility changes.
The runtime's startup `Public` check catches a private change made while the
process was down. REST message lists expose only messages that still exist;
they expose edited current revisions but no historical edit versions or
deletion tombstones.

Discord has no version number for a channel's permissions, so a channel that
changes visibility is the composed-token case: the connector re-emits the
channel's messages with a token of `perm:<hash of the channel's permission
overwrites>`, or `<edited_timestamp>+perm:<hash>` for a message that had also
been edited. Because the member set is a `group` entry rather than a list of
people, this is only needed when the *shape* of access changes — public to
private — not when somebody joins or leaves the channel.

One gap found while writing this, and the reason a tombstone is its own artifact.
Discord's `MESSAGE_DELETE` carries no timestamp for the deletion, so the obvious
shape — a tombstone timed by when the gateway event arrived — produces a
different native id every time the gateway replays after a reconnect, and
reconnects replay. Hanging the tombstone off a fixed artifact id,
`<message id>:tombstone`, makes a replayed deletion idempotent like everything
else.

### Google Drive (issue #15)

| Artifact | kind | artifact id | native_id | container |
|---|---|---|---|---|
| Document | `document` | `<file id>` | backfill: `<file id>@<head revision id>`; live: `<file id>@<head revision id>+perm:<observation version>` | folder `<folder id>` |
| Meeting transcript | `transcript` | `<file id>` | backfill: `<file id>@<head revision id>`; live: `<file id>@<head revision id>+perm:<observation version>` | folder |
| Comment on a document (future work) | `message` | `<file id>:comment:<comment id>` | `…@<modified time>` | folder |

The initial Drive backfill implements bounded `Backfiller` calls. It walks
one page of one explicitly configured folder per call. The source `containers`
are direct parent folder IDs; there is no recursive expansion. Candidate
transcript folders are separately listed in
`settings.transcript_candidate_folder_ids` and must also be in `containers`.
The remaining folders contain general documents. Candidate files are emitted
only when a directly applied label has the exact published ID configured in
`settings.meeting_transcript_label_id`; the human title does not participate.
Missing, different, duplicate or unreadable label metadata skips the file
without document fallback. Transient API failures retry the page.

The head revision ID is the content token. Drive returns `headRevisionId` for
binary files; for native Google Docs the connector takes the last ID in the
revisions list, which requires writer or owner access. A document edited ten
times can therefore have ten L0 events with one artifact id. ACL comes from
the file's current permissions:
`domain` sharing maps to `public`, a group permission to `group`, a per-person
share to `identity`. A transcript's author is often the meeting bot rather than a
person, which is why `transcript` does not require one; attendees go in
`participants` with role `attendee`.

The backfill uses the content head revision ID exactly, as issue #98 requires.
Ongoing sync consumes Drive's changes feed with a separate durable cursor.
Every change re-fetches the file, its direct labels and current permissions.
The live observation version hashes the normalized ACL, folder, metadata,
document kind, change observation token and, on re-entry, the latest retraction
event id. An unchanged observation emits nothing.
This lets sharing-only changes and a public → private → public cycle produce
distinct revisions. A new or expired change token triggers a full
reconciliation of configured folders against L0; the token captured before
that scan catches any concurrent changes afterward. An optional
`settings.notification_url` registers an expiring Drive change channel.
Verified notifications wake the poller; the configured refresh cadence polls
the same durable feed if notification delivery is unavailable or missed.

Files whose direct parent is not configured are ignored before reaching the
gate, which also enforces the source allowlist. Deletions, moves outside the
allowlist, and meeting-label removal emit a tombstone under the former folder
and ACL. Its native id includes the revision it retracts, making each cycle
distinct and its replay idempotent. Change notifications, polling and recovery
re-fetch the current file, so a stale removal notice cannot retract a file
that is now eligible. A move back or exact label reapplication creates a fresh
revision under the stable file id using current content and ACL. Until that
revision is observed, the file stays retracted; unreadable, absent or wrong
labels and out-of-folder files cannot restore it.

### Obsidian (issue #102)

| Artifact | kind | artifact id | native_id | container |
|---|---|---|---|---|
| Markdown note | `document` | `<vault-relative path>.md` (unsafe bytes percent-escaped) | `<path>@<SHA-256 content hash>+<permission hash>` | containing vault-relative folder |

A connector may implement `BackfillVersioner` with a stable `BackfillVersion()`
for its configured input set. The runtime stores that value in the completed
cursor and starts a new bounded walk after restart when it changes. An
unfinished walk remains resumable by its own cursor.

One locally mounted vault is one source. The source's explicit `containers`
are allowed folders and include descendants; the gate uses a folder boundary
for this source. Backfill visits a bounded set of filesystem entries per call
and stores the directory stack and last visited names in its cursor, so a
restart needs no process-local state. Symlinks are skipped. Markdown is kept
as-is in `text`; parsed YAML frontmatter is in `native`. The configured owner
is the author and the sole identity ACL entry by default. `public: true` is an
explicit opt-in. The permission hash includes the ACL, owner and configured
permission version, so a changed ACL with unchanged content is a new revision.
A return to an earlier ACL requires a new configured permission version.
A changed owner, ACL, root, folder allowlist or template list starts a new
backfill on restart; this is configuration handling, not live polling.
Polling reconciles a safe vault walk with the current document inventory in
L0 on every refresh. The inventory includes previously ingested folders no
longer allowed by configuration, so a restart still retracts removed notes.
An unchanged modification time skips reading a file; a changed time triggers
a content hash check before a revision is emitted. Missing, renamed, excluded
or newly disallowed notes receive a tombstone using their previous ACL. A
rename creates a new path artifact. The gate admits these retraction events
even when the old folder has left the allowlist; they contain no note text.

### Agent sessions (issue #149)

| Artifact | kind | artifact id | native_id | container |
|---|---|---|---|---|
| Session start | `agent_session` | `<session id>` | `<session id>@start` | stream `<agent id>` |
| Session end | `agent_session` | `<session id>` | `<session id>@end` | stream |
| Turn | `agent_turn` | `<session id>` | `<session id>@turn:<turn id>` | stream |
| Tool call | `tool_call` | `<session id>` | `<session id>@call:<call id>` | stream |

The `agent` connector (`internal/connector/agent`) is a `Pusher` that agents
post their own session events to, one event per `POST /hooks/<source id>`. It is
not a source that Hearsay reads. The source's own signature is the agent's API
bearer token, the one its principal names in `token_env`
([ADR-0014](adr/0014-api-callers-use-per-principal-bearer-tokens.md)):
`Authorization: Bearer <agent token>`. The author is the agent that token
belongs to, `{source, kind: agent, native_id: <agent id>}`. A body whose optional
`agent` names a different agent is refused. `on_behalf_of` names the person the
session acts for. It must be a configured human principal, and it becomes a
participant with role `author`, as it is on the API's own `audit` and
`assertion` events.

The artifact is the session, and each event is one revision of it, keyed by
what the event is. An artifact's history in L0 is therefore the session in
order. `time` is when the session started (`started_at`) on every event,
because every revision of an artifact carries the same `time`. When the event
itself happened (`time` in the request) is `payload.revision.edited_at`, and it
orders the session's events. Posting an event again writes nothing. Posting the
same key with different content is answered 409 and writes nothing, because a
key names one thing that happened. Session, turn and call ids are 1 to 128 bytes
of letters, digits, `-`, `_`, `.` and `:`.

The container is the agent's session stream, `{kind: stream, native_id: <agent
id>}`. The source's `containers` are `*` or the agent ids allowed to post. An
agent that is not allowed is refused with 403, so the event is not dropped
without the agent knowing. The ACL is two `identity` entries in this source, the
agent and the person, whose native ids are their principal ids. Nobody else is
on it. A turn's `text` is `payload.text`. The session id, phase, turn or call id,
tool name, and a tool call's `input` and `output` are in `payload.native`.
None of these kinds is distilled.

The responses are 202 with `{"id": "<event id>"}` for an event written or
already held, 400 for a request that is not a valid event, 401 for a missing
token or one that is not an agent's, 403 for a body naming another agent, a
person who is not a configured human, or an agent the source does not allow,
and 409 for a rewritten key. The requests of one session, one per kind:

```http
POST /hooks/agent-sessions
Authorization: Bearer <shed's token>
Content-Type: application/json

{"on_behalf_of": "kyle", "session": "s-01", "kind": "agent_session", "phase": "start",
 "started_at": "2026-09-23T17:00:00Z", "time": "2026-09-23T17:00:00Z"}
```

```json
{"on_behalf_of": "kyle", "session": "s-01", "kind": "agent_turn", "turn": "1",
 "started_at": "2026-09-23T17:00:00Z", "time": "2026-09-23T17:00:05Z",
 "text": "Reading the retry policy before changing it."}
```

```json
{"on_behalf_of": "kyle", "session": "s-01", "kind": "tool_call", "call": "c-1", "tool": "get_bundle",
 "started_at": "2026-09-23T17:00:00Z", "time": "2026-09-23T17:00:06Z",
 "input": {"scope": "api"}, "output": {"tokens": 1830}}
```

```json
{"agent": "shed", "on_behalf_of": "kyle", "session": "s-01", "kind": "agent_session", "phase": "end",
 "started_at": "2026-09-23T17:00:00Z", "time": "2026-09-23T17:10:00Z"}
```

The turn above is written as:

```json
{
  "id": "evt:agent-sessions:s-01@turn:1",
  "source": "agent-sessions",
  "native_id": "s-01@turn:1",
  "kind": "agent_turn",
  "time": "2026-09-23T17:00:00Z",
  "payload": {
    "artifact": "s-01",
    "container": {"kind": "stream", "native_id": "shed"},
    "text": "Reading the retry policy before changing it.",
    "author": {"source": "agent-sessions", "kind": "agent", "native_id": "shed"},
    "participants": [{"identity": {"source": "agent-sessions", "kind": "user", "native_id": "kyle"}, "role": "author"}],
    "revision": {"token": "turn:1", "edited_at": "2026-09-23T17:00:05Z"},
    "native": {"session": "s-01", "turn": "1"}
  },
  "acl": [
    {"kind": "identity", "source": "agent-sessions", "native_id": "shed"},
    {"kind": "identity", "source": "agent-sessions", "native_id": "kyle"}
  ]
}
```

## Changing this contract

The L0 event shape is a contract other work depends on (CONTRIBUTING.md).
Changing it — a new required field, a kind removed, a different id format — is a
design change that needs an ADR, not an edit here. Adding an optional payload
field or a core kind is additive and needs only this document and the code
updated together, in the same commit.
