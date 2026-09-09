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
a letter or a digit.

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
| `audit` | A bundle served: who asked, on whose behalf, what was filtered | yes | no |
| `tombstone` | An artifact deleted at the source | no | no |

`assertion` and `audit` are written by Hearsay itself rather than by a connector.
They are in the vocabulary because they are L0 events like any other.

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
| `container` | object | yes | `{kind, native_id, name?}` — the repository, channel or folder the artifact lives in. |
| `url` | string | where one exists | The permalink a human would follow. |
| `title` | string | where one exists | The artifact's own title: an issue title, a document name, a thread name. |
| `text` | string | where there is any | What a human reads in the source, as plain text or markdown, with the source's markup left alone. This is the distiller's input. |
| `author` | identity | see the kind table | Who produced the artifact, as the source names them. |
| `participants` | array | where the source lists them | `[{identity, role}]`, role one of `author`, `reviewer`, `attendee`, `agent`. |
| `mentions` | array of identities | where the source resolves mentions | Identities the text refers to. |
| `links` | array of strings | where the source gives them | URLs the artifact carries, verbatim. L1 turns them into typed references; a connector does not. |
| `parent` | artifact id | where there is a parent | The thing this one hangs off: the issue a comment is on, the message a reply answers. |
| `thread` | artifact id | where there is a thread | The root of the conversation, which is what the distiller assembles a thread from. On a two-level source it equals `parent`. |
| `revision` | object | exactly when `native_id` is `artifact@<token>` | `{token, edited_at?}`: `token` is that token, and `edited_at` is when *this revision* came about, which is what orders an artifact's revisions — see idempotency below. |
| `target` | artifact id | on tombstones only | The artifact the tombstone retracts. |
| `native` | any JSON | no | The source's own object, verbatim. Nothing above L0 reads it, and nothing the fields above ask for may be hidden in it. |

`parent`, `thread` and `target` reference **artifact ids**, never native ids: a
comment hangs off an issue, not off one revision of it.

Container kinds are `repository`, `channel`, `folder` and `workspace`; a source
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

## Idempotency, edits and deletions

Three ids, doing three different jobs:

- **artifact** — the thing. Stable for its whole life, across every edit and its
  deletion. `acme/api#12`, a Discord message id, a Drive file id.
- **native_id** — one *observation* of the thing. Equal to the artifact id for an
  artifact that cannot change, and `<artifact>@<revision token>` for one that
  can.
- **id** — Hearsay's id, derived from the source and the native id.

**Re-emitting is free, and is the expected behaviour.** Ingest is idempotent on
the event id: an event whose id is already in L0 writes nothing. A connector that
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
  re-emitting every artifact in it. That is what a re-sync is; the runtime (#8)
  schedules it like a backfill, and re-emitting an artifact whose permissions
  have not changed is deduplicated away, so an interrupted re-sync can be run
  again.
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
and walks provenance forward. A connector has nothing to do with it.

A tombstone carries the same ACL as the artifact it retracts, so that the
retraction is visible to exactly the people the artifact was.

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
least one of the two ingest modes, and the runtime refuses to start one that
implements neither:

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

// Optional: history.
type Backfiller interface {
    Connector
    Backfill(ctx context.Context, sink Sink, from Cursor) (BackfillResult, error)
}
```

Push where the source supports it, poll otherwise; a connector may do both, and a
source that pushes still needs `Backfiller` to get its history.

- **`Poll`** is never called concurrently with itself, so a poller may keep its
  position in memory without locking. It returns when it has emitted what one
  pass found. An error is retried on the next tick with backoff.
- **`Handler`** is mounted by the runtime under a path it owns. The handler
  verifies the source's own signature over the request — the runtime cannot, the
  scheme is the source's — and a delivery it cannot verify is rejected with
  nothing emitted.
- **`Backfill`** does a bounded amount of work per call — a page, a day, whatever
  the source's API pages by — and returns `{Next, Done, Events}`. The runtime
  stores `Next` and hands it back, so a backfill interrupted by a restart
  resumes. The first call gets the zero cursor. A `Cursor` is an opaque string
  that must survive a restart, so it may not refer to anything held in memory.
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
format belongs to the configuration work (issues #5 and #37); this is what
reaches the connector:

| Field | Meaning |
|---|---|
| `ID` | The source id. Goes in every event. |
| `Type` | The connector type, which selects the factory. |
| `Containers` | The repositories, channels or folders this source may ingest, by native id. Default deny; `*` widens it. |
| `Refresh` | How often the runtime calls `Poll`. Ignored by a push-only connector; the runtime applies its own floor and jitter. |
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

## The three connectors this was checked against

Written before the connectors, and checked on paper against the GitHub (v0.2.0),
Discord (v0.3.0) and Drive (v0.4.0) work.

### GitHub (issue #8)

| Artifact | kind | artifact id | native_id | container |
|---|---|---|---|---|
| Issue | `issue` | `acme/api#12` | `acme/api#12@<updated_at>` | repository `acme/api` |
| Issue or PR comment | `message` | `acme/api#12:comment:998` | `…@<updated_at>` | repository |
| Pull request | `pull_request` | `acme/api#31` | `…@<updated_at>` | repository |
| Review | `review` | `acme/api#31:review:77` | same — a submitted review does not change | repository |
| Review comment | `review_comment` | `acme/api#31:comment:88` | `…@<updated_at>` | repository |
| Commit on the default branch | `commit` | `acme/api@<sha>` | same — a commit's content does not change | repository |

`parent` is the issue or pull request for a comment or review, and `thread` is
the same: GitHub conversations are two levels. Author is the GitHub node id with
the login as `handle`. ACL is `public` for a public repository and
`{group, native_id: "acme/api"}` for a private one, which is the collaborator set
resolved at read time rather than a frozen list. Webhooks are `Pusher` with
signature verification in the handler; backfill is `Backfiller` over the REST
list endpoints with the page cursor. Deleting a comment sends
`issue_comment.deleted`, which is a `tombstone` with artifact
`acme/api#12:comment:998:tombstone` and `target` the comment's artifact id.

Every native id with an `@` carries `payload.revision.token` equal to the part
after it, so an issue at `acme/api#12@2026-09-09T12:00:00Z` has that timestamp as
its token. A repository going private is an ACL change with no content change,
and GitHub gives no version for visibility, so the connector composes the token
the way the general rule says: `<content token>+perm:private` where the artifact
has a content token, and `perm:private` alone where it does not. Reviews and
commits are the second case — a review carries `submitted_at` and never changes,
a commit's content never changes — so `acme/api@<sha>` re-emits as
`acme/api@<sha>@perm:private`. Without that clause those two rows would re-emit
with their original native ids, deduplicate to nothing, and keep `public` on
every commit in a repository that is no longer public.

Checks out. The one thing worth naming: an edit to an issue body arrives with the
same `updated_at` granularity as a label change, so a connector emitting on every
`issues` webhook produces revisions whose content is identical. That is
acceptable — a revision that changes nothing is a row in L0, and the distiller
regenerates from the latest — but a connector that wants to avoid it hashes the
content into the revision token instead of using `updated_at`.

### Discord (issue #13)

| Artifact | kind | artifact id | native_id | container |
|---|---|---|---|---|
| Message | `message` | `<message id>` | `<id>@<edited_timestamp>` when edited | channel `<channel id>` |
| Thread | `thread` | `<thread id>` | same | channel `<parent channel id>` |
| Reaction | `reaction` | `<message id>:reaction:<user id>:<emoji>` | same | channel |
| Deleted message | `tombstone` | `<message id>:tombstone` | same, with `target` `<message id>` | channel |

Discord gives `edited_timestamp` as `null` until a message is edited, which is
exactly the revision token this contract asks for. Threads are their own channel
ids, so a message in a thread has `container` = the parent channel (what the
allowlist names) and `thread` = the thread's artifact id — the distinction the
contract draws between container and thread is load-bearing here. Reactions carry
their author and no text, which is why `reaction` requires neither. ACL for an
allowlisted private channel is `{group, native_id: <channel id>}`, so the member
set is resolved at read time.

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
| Document | `document` | `<file id>` | `<file id>@<head revision id>` | folder `<folder id>` |
| Meeting transcript | `transcript` | `<file id>` | `<file id>@<head revision id>` | folder |
| Comment on a document | `message` | `<file id>:comment:<comment id>` | `…@<modified time>` | folder |

Drive's revision ids are what the contract's revision token was designed for, and
a document edited ten times is ten L0 events with one artifact id. Change
notifications are a `Pusher`, with a `Poller` fallback for the folders Drive will
not watch — a connector implementing both is why the ingest modes are separate
interfaces rather than a mode field. ACL comes from the file's permissions:
`domain` sharing maps to `public`, a group permission to `group`, a per-person
share to `identity`. A transcript's author is often the meeting bot rather than a
person, which is why `transcript` does not require one; attendees go in
`participants` with role `attendee`.

Drive is the source where the ACL moves most and the composed token earns its
keep: sharing a document changes the permission list and not the head revision
id, so the token is `<head revision id>+perm:<permission list etag>`, and the
re-share lands as a new revision with the same content and a new `acl`.

Two things Drive needs that the contract deliberately leaves to the connector: a
file that moves between folders changes container, and the connector emits the
next revision under the new container rather than rewriting history; and a file
in a folder that is not allowlisted is dropped by the gate, so a connector
watching a whole drive is safe by construction.

## Changing this contract

The L0 event shape is a contract other work depends on (CONTRIBUTING.md).
Changing it — a new required field, a kind removed, a different id format — is a
design change that needs an ADR, not an edit here. Adding an optional payload
field or a core kind is additive and needs only this document and the code
updated together, in the same commit.
