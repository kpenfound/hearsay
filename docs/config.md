# Hearsay configuration

What Hearsay ingests, what a scope is, who people are, and who may decide
things. It is a directory of YAML applied like GitOps — checked in, reviewed,
deployed — or a single file that says the same thing, for a team that does not
need five directories yet.

This document is the schema. [ADR-0009](adr/0009-configuration-as-a-gitops-directory.md)
is why it is shaped this way, [design.md](design.md#configuration) is what it is
for, and `internal/config` is the loader that reads it.

## The two forms

```
config/                     config/hearsay.yaml
  sources/*.yaml
  scopes/*.yaml       or    (one file with a
  principals/*.yaml          sources:, scopes:, principals:,
  code/*.yaml                code: and authority: key)
  authority/*.yaml
```

They are the same configuration. The single file's `sources:` list is what the
files in `sources/` hold between them, and so on for the other four: expanding
one form into the other is moving lists between files, and nothing else changes
— not a default, not a rule, not an id.

Start with the single file. Split it when a directory of it gets long enough
that two people editing it collide, which is the only thing the split buys.

Point Hearsay at either:

```sh
hearsay api --config ./config             # a directory
hearsay api --config ./hearsay.yaml       # a single file
hearsay config validate ./config          # check it without starting anything
HEARSAY_CONFIG=./config hearsay all       # the flag also reads the environment
```

A directory holding a `hearsay.yaml` and none of the five directories is the
single-file form; pointing `--config` at the directory finds the file. The file
may also be named `hearsay.yml`. Two things about such a directory are errors
rather than a guess: holding both forms, because nothing says which of the two a
source came from; and holding both `hearsay.yaml` and `hearsay.yml`, because
only one of them would be read and the other would go missing in silence.

**Rules that apply to every file:**

- A file in one of the five directories holds one object or a list of them, so
  a team can keep a file per source or one file for all of them. A file that
  holds several YAML documents separated by `---` is an error; use a list.
- Unknown fields are an error, not a warning. A misspelled key that is ignored
  in silence is the failure this format exists to avoid.
- Files that are not `.yaml` or `.yml` are ignored, so a README belongs in a
  configuration repository. A directory that is not one of the five is an
  error, and so is a subdirectory inside one of them: reading none of it
  silently is how configuration goes missing.
- An empty file is an error. It is either a half-finished edit or a file
  somebody meant to delete.

## When configuration is read

**At startup, once.** Every process reads it, validates it and either starts or
refuses. Nothing polls, nothing watches, and there is no reload signal
(ADR-0009). A configuration change is a deploy: change the repository, let
`hearsay config validate` gate the merge, roll the processes.

Every process logs `configuration loaded` at startup with a `config_digest`,
which `hearsay config validate` prints too. Two processes reporting the same
digest are running the same configuration; a process whose digest is not the one
in the configuration repository has not been rolled yet. That is how "restart to
reload" is checkable rather than assumed.

## `sources/`

One entry per configured *source instance*: not a vendor, but one account, one
workspace, one drive. `github-oss` and `github-acme` are two sources served by
one connector type. The id goes into every event the source produces and is not
renamed afterwards ([connector contract](connector-contract.md#source-ids)).

```yaml
- id: github                 # required; 1-64 bytes, lowercase, digits, - and _
  type: github               # required; selects the connector
  containers:                # required; the ingest allowlist, default deny
    - acme/api
    - acme/infra
  refresh: 2m                # optional; how often a polling connector polls
  settings:                  # optional; the connector's own configuration
    include_forks: false
  secrets:                   # optional; names of environment variables
    token: HEARSAY_GITHUB_TOKEN
```

| Field | Meaning |
|---|---|
| `id` | The source id. It appears in every event id, so it is chosen once. |
| `type` | The connector type: `github`, `discord`, `drive`, or a third party's. |
| `containers` | The repositories, channels or folders this source may ingest, **by native id** — a repository full name, a channel id, a folder id, never a display name. This is control point 1 of [access control](design.md#access-control): default deny, so a container that is not listed is not ingested. `*` widens it to everything the credentials can see, and must then be the only entry. |
| `refresh` | A duration (`30s`, `5m`, `1h`). Ignored by a connector that only receives pushes; the runtime applies its own floor and jitter. |
| `settings` | Opaque to Hearsay and passed to the connector, which rejects a field it does not have. What belongs here is documented by the connector. |
| `secrets` | A map from the name the connector asks for to **the name of an environment variable**. A value that is not an environment variable name is an error, because a configuration repository is checked in and a token pasted here would be too. |

## `scopes/`

A scope is a named bundle of sources: what is *relevant* to a piece of work.
Scopes filter for relevance and ACLs grant permission, and they are never the
same mechanism — a scope never widens what a principal may read.

```yaml
- id: api                    # required
  name: Acme API             # optional; what people call it
  sources:                   # required; at least one
    - github                 # a source id on its own takes everything it ingests
    - source: discord        # or a mapping, to take some of its containers
      containers: ["824100000000000001"]
  tracker:                   # optional
    source: github
    project: acme/api
  entities:                  # optional; code entity ids this scope is about
    - code:acme/api
```

`tracker` is the mapping the design doc asks for: which tracker this scope's
`tracker_item` entities come from. Item `1234` of the scope above is entity
`tracker:github:acme/api#1234` — `tracker:<source>:<project>#<item>`. The source
id rather than a vendor abbreviation, so two trackers with the same project key
in different sources stay apart and an entity id reads back to the source that
owns it without a lookup, the way an event id does.

A scope's tracker must be a source the scope covers: a tracker whose items the
scope never ingests would map nothing. A container the source does not ingest is
an error too, for the same reason — the scope would be filtering for something
that is never there.

## `principals/`

Who someone is, in every source they appear in. Without this a stance has no
consistent author and authority cannot be checked, which is why this is the
part of onboarding worth spending effort on.

```yaml
- id: kyle                   # required; the id stances, owners and authority use
  name: Kyle Penfound        # optional
  kind: human                # optional; human (the default), agent or team
  identities:                # required on a human and an agent; at least one
    - source: github
      native_id: "MDQ6VXNlcjE="
      handle: kpenfound
    - source: drive
      handle: kyle@acme.example

- id: shed
  kind: agent
  class: worker              # required on an agent: observer, worker, orchestrator, steward
  identities:
    - source: github
      handle: "shed-agent[bot]"

- id: api-team
  kind: team
  members: [kyle, shed]      # principal ids; a team may not contain a team
```

An identity needs a `native_id`, a `handle`, or both.

- `native_id` is the source's stable id — a GitHub node id, a Discord user id, a
  Google account id. It survives a rename, so it is the key to match on. It is
  matched byte for byte: a GitHub node id is base64, and its case is meaning.
- `handle` is the login, @-name or email address the source shows: what a person
  can actually type. A handle-only identity stops matching the day its owner
  renames themselves. That is a real cost, and it is still the right default for
  onboarding — write handles to get started, and fill in native ids for the
  people whose history matters. Handles are matched ignoring case and
  surrounding space, so `KPenfound` and `kpenfound` are one handle.

Two principals may not claim the same identity in the same source.

### The id, and how it is minted

A principal id is written by hand, here, and nothing derives it from a source: a
principal outlives any one source's idea of who they are. It is 1 to 64 bytes of
lowercase letters, digits, `-` and `_`, starting with a letter or a digit — the
same shape as a source id and a scope id, so there is one rule for every name a
person types into configuration.

It is then used bare wherever a principal is named: a stance's author, an
`owners` entry, an authority policy, an L1 participant, and an L2 entity of type
`person` or `team`, which carry the type beside the id rather than in it
([design](design.md#l1-distilled-documents)). This is the difference from a code
entity's `code:` or a tracker item's `tracker:<source>:<project>#<item>` — those
name something *inside* a source and need a namespace to stay apart. A principal
id names nothing inside a source, so it has no prefix and there is nothing to
derive.

### The three kinds

`human` is a person, and is the default because most principals are people.

`agent` is an agent — agents are principals too, and say what they said the same
way a person does. An agent's `class` is what it may read and write
([access control](design.md#access-control)) and is required; a human and a team
do not have one.

`team` is a group. A team owns code entities and stands in a source's ACLs as a
group; it never authors anything, because a team cannot say something — one of
its members does. A team says who it is in one of two ways, and may use both:

- `members`, a list of principal ids. A team may not contain a team, so this is
  a list and not a hierarchy: nest teams in the source, and list the people here.
- `identities`, when the source has the group itself — a GitHub team, a Discord
  role. Then the source keeps the membership and the mapping only has to name
  it, which is what makes a team worth configuring at all.

A team with neither names nobody and is rejected.

A group is looked up by both keys, like anyone else, because a source names one
both ways: a GitHub team is a node id in an ACL and the slug `acme/api-team` in
CODEOWNERS. Writing only the slug is enough, and is what onboarding produces.
Write the group as the source spells it — `acme/api-team`, not the
`@acme/api-team` a CODEOWNERS line carries, the way a tracker item is `1234` and
not `#1234`.

### How an identity resolves

Every event a connector emits carries identity hints: who wrote the artifact,
who else took part, who was mentioned. A hint is what the source says — a native
id, a handle, sometimes an email address — and never a Hearsay principal id
([the connector contract](connector-contract.md)). Turning one into the other is
the resolver's job, and it works from what is written here.

- **Matching is per source.** A handle in one source is no evidence about a
  handle in another, so a person is listed once per source they appear in.
- **Three keys.** The hint's native id, its handle, and its email address, which
  is matched against handles — an identity in `drive` or a calendar attendee is
  written as an email in `handle`, because that is what a person can type.
- **Every key that matches counts.** Keys that agree are one match. Keys that
  name *different* principals are ambiguous, and the identity is left unresolved
  rather than decided by preferring the stable id: they disagree only when this
  file has fallen behind a rename in the source, and quietly preferring one key
  would hide that for good.
- **Nothing is silently dropped.** An identity that is unknown or ambiguous is
  recorded for a person to map. The event keeps the hint — L0 is append-only —
  so authorship comes back on its own once the mapping is fixed and the
  documents are distilled again. No placeholder principal is invented.
- **That record is bounded**, at 4096 distinct identities per process. Its keys
  come from sources, so an unbounded one would hold every bot that ever posted.
  Past the bound a process counts the sightings it dropped instead of keeping
  them, and reports the count beside the list: a non-zero count says the list is
  a sample and the mapping is a long way behind. The events themselves are
  untouched either way, so nothing is lost that re-distilling cannot recover.

### Agent classes

An agent's class is the ceiling on what it may do, from
[access control](design.md#access-control). The four are a chain: each may do
everything the one before it may.

|Class|Reads|Writes|
|---|---|---|
|`observer`|the L1 and L3 of the scopes it is granted|nothing|
|`worker`|its scopes, plus the code entities they link to|`assert` (proposals)|
|`orchestrator`|the same, and it is usually granted several scopes|`assert`, subscribe|
|`steward`|everything ingested|ratify, merge topics|

An agent never gets more than the person it is acting for: a read runs as the
intersection of the agent's grants and theirs. Ratifying and merging topics are
human actions, and an agent does not get them from the steward class alone — the
class exists so the door is there, closed. Which scopes a principal is granted
is not configured here yet; enforcement lands in v0.2.0 and v0.6.0.

## `code/`

The map from how people talk about code to where the code is and who owns it.
Hearsay holds no code: an entity is vocabulary, path patterns and ownership.

```yaml
- id: code:acme/api:engine/server   # required; starts with `code:`
  type: service                     # required; project, service, module or symbol
  name: Engine server
  aliases: [engine, the engine]     # what people call it, matched ignoring case
  path_patterns: [engine/server/**] # resolved against HEAD at read time
  part_of: [code:acme/api]          # the hierarchy, seeded
  owners: [kyle]                    # principal ids
  repo:                             # required when path_patterns are set
    source: github
    project: acme/api
  codeowners: .github/CODEOWNERS    # optional; seeds owners for descendants
```

Path patterns rather than file lists, because a refactor should not invalidate
the map. `part_of` may not form a cycle: stances inherited from ancestors are a
walk up this hierarchy, and a cycle makes it a walk with no end. An alias may
only mean one thing — an alias that resolves to two entities resolves to
neither.

`codeowners` records where to import ownership from. The import itself lands
with the code entity work; config is where the intent is written down.

## `authority/`

Which principals and which sources can produce **ratified** stances, per scope,
and how sources rank against each other when stances disagree. This is the
design doc's open question answered, and it is the most consequential thing in
the configuration: agents act on ratified stances without asking, cite inferred
ones and proceed, and stop on contested ones
([topics and stances](design.md#topics-and-stances)).

```yaml
- scope: "*"                 # the default every other scope inherits
  ratified_by:
    principals: [kyle, robin]

- scope: api                 # an override for one scope: this team decides in
  ranking:                   # meetings, so a meeting outranks a merged PR
    - meeting
    - merged_pr
    - spec
    - issue
    - pull_request
    - commit
    - chat_thread
    - dm
    - agent
  ratified_by:
    artifacts: [merged_pr, spec]
```

**Composition.** A scope's policy inherits every field it does not set from the
`*` policy, which inherits every field *it* does not set from the built-in
default below. A field that is set **replaces** what it inherited; lists are
never appended to. A half-inherited ranking is not readable from the file in
front of you, and being able to read the policy off one file is the point.

A list that is present but empty inherits nothing: `principals: []` means nobody
may ratify by hand in this scope. `ranking: []` is an error rather than "nothing
outranks anything" — leave the field out to inherit.

**A ranking that leaves a class out puts it below every class it lists.** The
omitted classes tie with each other, so between two of *them* nothing outranks
anything and the more recent stance decides — but every class in the list beats
all of them, `agent` included. Leaving a class out is worse for it than ranking
it last. Writing `ranking: [merged_pr, meeting]` is therefore a decision about
all nine classes and not just two: it puts the other seven, `agent` among them,
level with each other and below both. Write the whole order out when you mean
the whole order — the example above overrides one position and still lists nine.

A class that ratifies **on its own** has to be in the ranking in force for the
scope, and the loader refuses a policy where it is not. `artifacts: [spec]`
under a ranking that omits `spec` says a spec is authoritative enough to settle
a topic with no human in the loop and also loses every disagreement it is in,
which is not a policy anyone means to write. Ranking is inherited as readily as
ratification, so this is checked on the policy as it ends up rather than on one
file — narrowing a scope's ranking can contradict a `ratified_by` it inherited
from `*`. It is reported against the file that **set** one of the two, which is
where the contradiction was introduced; a scope that inherited both halves is
not told about a mistake it has no part in.

There is at most one policy per scope, and at most one `*`. Two policies for one
scope would be two answers.

### Artifact classes

Authority is a judgement about *where* something was said. The classes are
coarser than an L0 kind and finer than a source, because a merged pull request
and an open one are the same kind of thing with very different weight.

| Class | What produces it |
|---|---|
| `merged_pr` | A pull or merge request that was merged. |
| `spec` | A wiki page, design doc or ADR — something written to be referred back to. |
| `meeting` | A segment of a meeting transcript. |
| `issue` | A tracker item and its comments. |
| `pull_request` | A change proposal that was not merged, and the reviews on it. |
| `commit` | A commit on a watched branch. |
| `chat_thread` | A thread or burst in an ingested channel. |
| `dm` | A direct message. DMs are not ingested unless a person lists the container. |
| `agent` | An agent turn, or a stance an agent wrote through `assert`. |

The distiller assigns a class to every L1 document it produces, from the
document's kind and the state of the artifact behind it: `pr` is `merged_pr`
when it merged and `pull_request` otherwise, `meeting_segment` is `meeting`,
`wiki_section` is `spec`, `issue` is `issue`, `commit` is `commit`,
`slack_thread` and `slack_burst` are `chat_thread` unless the container is a
direct-message container, and `agent_turn` is `agent`. That is a contract on L1,
written here because authority is what needs it.

### The built-in default

```yaml
- scope: "*"
  ranking:
    - merged_pr              # the team did not merely say it, they shipped it
    - spec                   # written to be referred back to
    - meeting                # the team talking, with a record
    - issue                  # written down, and about one thing
    - pull_request           # proposed, not agreed
    - commit                 # a decision's trace rather than its statement
    - chat_thread            # said in passing
    - dm                     # said in passing, to one person
    - agent                  # agents propose, people decide
  ratified_by:
    principals: ["*"]        # anybody who can write to the scope may ratify
    sources: ["*"]           # from any source
    artifacts: [merged_pr]   # and a merged PR ratifies on its own
```

The design doc fixes four of the nine positions — a merged PR outranks a
meeting, which outranks a chat thread, which outranks a DM — and the rest follow
the same principle: something written down deliberately outranks something said
in passing.

`ratified_by` has three parts and they are not the same rule:

- `principals` is who may ratify **by hand**, with the reaction or the slash
  command. `*` is anyone who can write to the scope.
- `artifacts` is what ratifies **on its own**, with no human in the loop.
- `sources` restricts where such an artifact may come from. It applies to the
  `artifacts` rule only: a person ratifying is a person, not a source. Use it
  when one source is authoritative and another is a mirror.

A frozen spec and CODEOWNERS are named alongside a merged pull request in the
design doc, but nothing can tell a frozen spec from a draft one, so `spec` is a
deliberate one-line override rather than a default.

## Validation

```sh
hearsay config validate            # the working directory
hearsay config validate ./config
```

It reports **everything** wrong, not the first thing, with the file, the line
the object starts on, and the field:

```
hearsay: config: ./config is not a valid configuration: 3 problems
  scopes/api.yaml:6: scope "api": sources[1].source: no source is configured with id "discrod"
  scopes/api.yaml:9: scope "api": tracker.project: source "github" does not ingest "acme/apiv2"
  principals/people.yaml:4: principal "kyle": identities[0].source: no source is configured with id "gh"
```

A file that does not parse, or that has a key which is not a field, is reported
on its own: what such a file was meant to say is unknown, so nothing else can be
checked against it. Fix those, run it again, and the rest of the configuration
is checked in one pass.

Beyond the per-field rules above, the loader checks what only makes sense across
objects: every reference resolves (a scope's sources, a policy's principals, an
owner, a team's members, a parent entity), no two objects share an id, no two
principals claim one identity, no team contains a team, no alias means two
things, `part_of` has no cycles, and there is at
least one source and one scope — a configuration with neither ingests nothing
and serves nothing.

An object whose own id is missing or malformed is reported *and* treated as not
being there, so a scope naming a source called `GitHub` is told both that
`GitHub` is not a source id and that nothing of that name is configured, in one
pass rather than in two. A duplicate id is the other way round: something of
that name does exist, so only the collision is reported and everything pointing
at it is left alone.

Run it in CI on the configuration repository. It needs no database, no network
and no credentials, so it is a fast gate on a pull request.

## A complete example

A team using GitHub, Discord and Google Drive. First as one file:

<!-- example: single/hearsay.yaml -->
```yaml
# Hearsay configuration for the Acme API team.
sources:
  - id: discord
    type: discord
    containers:
      - "824100000000000001" # #eng
      - "824100000000000002" # #eng-infra
    secrets:
      token: HEARSAY_DISCORD_TOKEN

  - id: drive
    type: drive
    containers:
      - 1AbCdEfGhIjKlMnOpQrStUvWxYz # the Engineering folder
    refresh: 15m
    settings:
      # Whatever the Drive connector documents; Hearsay passes it through.
      recursive: true
    secrets:
      credentials: HEARSAY_DRIVE_CREDENTIALS

  - id: github
    type: github
    containers: [acme/api, acme/infra]
    refresh: 2m
    secrets:
      token: HEARSAY_GITHUB_TOKEN

scopes:
  - id: api
    name: Acme API
    sources:
      - github
      - source: discord
        containers: ["824100000000000001"]
      - drive
    tracker:
      source: github
      project: acme/api
    entities: [code:acme/api, code:acme/api:engine/server]

principals:
  - id: kyle
    name: Kyle Penfound
    identities:
      - source: github
        native_id: MDQ6VXNlcjE=
        handle: kpenfound
      - source: discord
        native_id: "302100000000000003"
        handle: kyle
      - source: drive
        handle: kyle@acme.example

  - id: robin
    name: Robin Okonkwo
    identities:
      - source: github
        handle: robinok
      - source: discord
        handle: robin

  - id: shed
    name: shed
    kind: agent
    class: worker
    identities:
      - source: github
        handle: shed-agent[bot]

  # The GitHub team owns the engine, so the source keeps the membership and
  # this only has to name it.
  - id: api-team
    name: API team
    kind: team
    identities:
      - source: github
        native_id: MDQ6VGVhbTE=
        handle: acme/api-team

code:
  - id: code:acme/api
    type: project
    name: API
    aliases: [api, the api]
    repo:
      source: github
      project: acme/api
    codeowners: .github/CODEOWNERS

  - id: code:acme/api:engine/server
    type: service
    name: Engine server
    aliases: [engine, the engine, engine server]
    path_patterns: [engine/server/**]
    part_of: [code:acme/api]
    owners: [kyle, api-team]
    repo:
      source: github
      project: acme/api

authority:
  - scope: "*"
    ratified_by:
      principals: [kyle, robin]

  - scope: api
    # This team decides in meetings, so a meeting outranks a merged PR here.
    # All nine classes are listed: one left out would rank below every one that
    # is, `agent` included.
    ranking:
      [meeting, merged_pr, spec, issue, pull_request, commit, chat_thread, dm, agent]
    ratified_by:
      artifacts: [merged_pr, spec]
```

And the same thing as a directory. The lists move into files, one object or many
per file, and nothing else changes:

<!-- example: dir/sources/discord.yaml -->
```yaml
# sources/discord.yaml — one source, so one mapping rather than a list.
id: discord
type: discord
containers:
  - "824100000000000001" # #eng
  - "824100000000000002" # #eng-infra
secrets:
  token: HEARSAY_DISCORD_TOKEN
```

<!-- example: dir/sources/drive.yaml -->
```yaml
# sources/drive.yaml
id: drive
type: drive
containers:
  - 1AbCdEfGhIjKlMnOpQrStUvWxYz # the Engineering folder
refresh: 15m
settings:
  # Whatever the Drive connector documents; Hearsay passes it through.
  recursive: true
secrets:
  credentials: HEARSAY_DRIVE_CREDENTIALS
```

<!-- example: dir/sources/github.yaml -->
```yaml
# sources/github.yaml
id: github
type: github
containers: [acme/api, acme/infra]
refresh: 2m
secrets:
  token: HEARSAY_GITHUB_TOKEN
```

<!-- example: dir/scopes/api.yaml -->
```yaml
# scopes/api.yaml
id: api
name: Acme API
sources:
  - github
  - source: discord
    containers: ["824100000000000001"]
  - drive
tracker:
  source: github
  project: acme/api
entities: [code:acme/api, code:acme/api:engine/server]
```

<!-- example: dir/principals/people.yaml -->
```yaml
# principals/people.yaml — several objects, so a list.
- id: kyle
  name: Kyle Penfound
  identities:
    - source: github
      native_id: MDQ6VXNlcjE=
      handle: kpenfound
    - source: discord
      native_id: "302100000000000003"
      handle: kyle
    - source: drive
      handle: kyle@acme.example

- id: robin
  name: Robin Okonkwo
  identities:
    - source: github
      handle: robinok
    - source: discord
      handle: robin
```

<!-- example: dir/principals/agents.yaml -->
```yaml
# principals/agents.yaml — agents are principals too.
- id: shed
  name: shed
  kind: agent
  class: worker
  identities:
    - source: github
      handle: shed-agent[bot]
```

<!-- example: dir/principals/teams.yaml -->
```yaml
# principals/teams.yaml — the GitHub team owns the engine, so the source keeps
# the membership and this only has to name it.
- id: api-team
  name: API team
  kind: team
  identities:
    - source: github
      native_id: MDQ6VGVhbTE=
      handle: acme/api-team
```

<!-- example: dir/code/api.yaml -->
```yaml
# code/api.yaml
- id: code:acme/api
  type: project
  name: API
  aliases: [api, the api]
  repo:
    source: github
    project: acme/api
  codeowners: .github/CODEOWNERS

- id: code:acme/api:engine/server
  type: service
  name: Engine server
  aliases: [engine, the engine, engine server]
  path_patterns: [engine/server/**]
  part_of: [code:acme/api]
  owners: [kyle, api-team]
  repo:
    source: github
    project: acme/api
```

<!-- example: dir/authority/default.yaml -->
```yaml
# authority/default.yaml
scope: "*"
ratified_by:
  principals: [kyle, robin]
```

<!-- example: dir/authority/api.yaml -->
```yaml
# authority/api.yaml
# This team decides in meetings, so a meeting outranks a merged PR here.
# All nine classes are listed: one left out would rank below every one that is,
# `agent` included.
scope: api
ranking:
  [meeting, merged_pr, spec, issue, pull_request, commit, chat_thread, dm, agent]
ratified_by:
  artifacts: [merged_pr, spec]
```

Both examples are loaded by the tests in `internal/config`, which check that
they are valid and that the two forms produce the same configuration. An example
here that stops working stops the build.

## Not in this version

Named deliberately, so that the absence is a decision rather than an oversight.

- **Nested teams.** A team may not contain a team. GitHub and Discord both nest
  groups; flattening them here keeps membership a list rather than a second
  hierarchy to walk and to check for cycles. Claim the group as an identity and
  let the source do the nesting.
- **Per-principal grants.** Which scopes a principal may read is part of the
  identity model but is not configured here yet: only the agent class ceiling
  is. Enforcement, and the grants it reads, land in v0.2.0 and v0.6.0.
- **Reload without a restart.** ADR-0009 has the reasons and what would have to
  be true first.
- **Per-scope ACLs.** Access control is inherited from the source at ingest, not
  configured here. A scope filters for relevance, and that is all it does.
- **Anchors.** A scope's anchors are pinned by people in the tools they are
  already in, or inferred; configuring them would be a second place they come
  from ([human feedback loop](design.md#human-feedback-loop)).
- **Secrets.** Configuration names an environment variable; it never holds a
  value. How that variable is populated is the deployment's business.
