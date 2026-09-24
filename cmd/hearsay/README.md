# cmd/hearsay

The one binary Hearsay ships. Each process is a subcommand of it (ADR-0003).

| Subcommand | What it runs |
|---|---|
| `hearsay connectors [--source name]...` | Source connectors. Writes L0 only. Serves webhooks and health on `--listen`. |
| `hearsay distiller` | The distiller. L0 to L1. |
| `hearsay assert-worker` | The assertion worker. L1 to L2. |
| `hearsay api` | The read and assert API. |
| `hearsay all` | All four in one process. Local development only. |
| `hearsay init [--github-repo owner/name]... [--discord-guild id --discord-channel id...] [--drive-folder id]... [--force]` | Write a new team's single-file `hearsay.yaml` and a mode-0600 env file for its secrets. Prompts on a terminal; every prompt has a flag. No database. |
| `hearsay config validate [path]` | Check a configuration repository. Prints every problem, with file and line. |
| `hearsay migrate up\|status\|up-to <n>\|down` | Schema migrations, then exit. `down` refuses without `--i-know`. |
| `hearsay l0 list\|get <id>\|count\|tail` | Inspect the L0 event store. Read-only. |
| `hearsay aliases list\|confirm <entity> <name>\|reject <entity> <name>` | List candidates and decide their names as a configured human. |
| `hearsay topics list <scope>\|merge <from> <into>\|split <topic> --stance <id>... --name <name>\|undo <op-id>\|ops` | List a scope's topics and the topic ledger; merge, split and undo as a configured human. |
| `hearsay gestures ratify\|demote\|pin\|unpin <document>\|undo <gesture-id>\|list` | Record and list human stance and pin gestures. |
| `hearsay eval [--since <RFC3339>] [--until <RFC3339>] [--scope <id>] [--json]` | The evaluation metrics over a window: time to ratification, topic merge and split rate, drill-down rate per bundle section, conflict-flag precision and next actions. Read-only; aggregates and ids only. |
| `hearsay delete --event <l0-id>\|--artifact <source> <artifact-id>\|--author <identity> [--apply]` | Preview deletion provenance; with `--apply`, redact L0 and re-distill what depended on it. |
| `hearsay delete list\|show <deletion-id>` | The deletions applied and what each rebuilt. Read-only. |
| `hearsay version` | Version, commit and build date. |

`hearsay aliases` requires `--config` and `--principal <human-id>`. Its list shows
entity, name, state and vote count only where that human may read every current
evidence document. Confirm and reject take the entity id and name shown by list.

`hearsay topics` requires `--config` and `--principal <human-id>`, a configured
human; an unknown id, an agent or a team is refused before the database is
opened. It reads what the API's `stance_history` would serve that person: the
topics as the topic ledger makes them now, within their reach, where the current
access lists of the evidence allow them
([ADR-0021](../../docs/adr/0021-reads-follow-the-topic-ledger.md)). A topic, a
stance or an operation they may not read is refused with the same message as
one that does not exist.

- `list <scope>` prints each readable topic in the scope, oldest first: id,
  the number of stances in its history the person may read, and name. A scope
  with nothing readable prints the header alone, as an unknown scope does.
- `merge <from> <into>` folds the first topic into the second; `split <topic>
  --stance <id>... --name <name>` moves the named stances, each one the person
  may read on that topic, onto a new topic; `undo <op-id>` reverses one merge
  or split. Each prints the id of the operation it recorded and nothing else.
  They go through `l2.Operate`
  ([ADR-0020](../../docs/adr/0020-topic-operations-are-a-ledger-held-on-the-scope-key.md)),
  which refuses a person the scope's authority policy does not let ratify by
  hand (`ratified_by.principals`). An undo that later operations in force are
  in the way of fails naming the ones the person may read. There is no dry run:
  every operation can be undone.
- `ops [--scope <scope>] [--since <RFC3339>] [--json]` prints the ledger the
  person may read, oldest first: id, time, kind, scope, principal, the
  operation an undo reverses, the undo that reversed it (`-` for none), the
  topics it covers, the covered stances they may read, and a split's new name.
  An operation is readable when every topic it covers reads, now, as a topic
  they may read. `--json` prints an array of objects with the same fields
  (`undone` is true once an undo reversed it).

`hearsay gestures` requires `--config` and `--principal <human-id>`. `ratify`,
`demote`, `pin` and `unpin` take one L1 document id, or `--artifact <source>
<artifact-id>` in place of the id. An artifact gesture covers every L1 document
made from that artifact. Ratify and demote cover every live stance drawn from
the document; pin and unpin act on its scope. The scope's
`ratified_by.principals` policy authorizes each write. Every write prints its
gesture record id; `undo <gesture-id>` reverses a gesture. `unpin` undoes all
active pins on the document and prints each undo record id. Missing and unreadable targets or records
are reported alike. `list [--scope <scope>] [--since <RFC3339>] [--json]`
shows the ledger oldest first, within the person's current reach and evidence
access. Stance ids the person cannot read are omitted from the listing.
Topic merge remains in `hearsay topics merge`.

`hearsay eval` requires `--config`, because a topic's standing is computed
under the configured authority, and takes no principal: it prints only counts,
durations, rates and scope and topic ids, never a stance's position, a topic's
name, L1 text or an L0 payload. The window is `--since` (default: the
beginning) up to but not including `--until` (default: now); `--scope` narrows
it to one topic scope key, and the bundles and next actions to the bundle scope
of that name. It takes no action word.

- *Time to ratification* replays each topic, as the topic ledger makes it now,
  through `l2.Stand`: every stance, gesture and undo recorded before `--until`,
  and each document's class as L1 holds it from the time the assertion worker
  read that version. A topic's clock starts when it first stands at a stance
  not served as ratified, and a topic counts in the window its clock started
  in. It prints how many clocks started, how many stood ratified before
  `--until` and how many had not (with their share), the median and p90 of
  the durations (nearest rank), and how many topics stood ratified from the
  moment they first stood.
- *Topic merge and split rate* counts the merges and splits recorded in the
  window, per topic row the assertion worker opened in it (a split's topic is
  not one), and beside each count how many an undo recorded before `--until`
  reversed. Undone operations are in the count, not netted out.
- *Drill-down rate per bundle section* reads the bundle audits linked to a
  session and the handle audits of that session. For each section it prints
  in how many of the bundles that served something in it a `get_l1`, `get_l0`
  or `stance_history` on one of its ids, or a `search` within it, followed
  before the next bundle on that scope in the session. It prints how many
  bundles had no session and were left out, and, as "looked beyond the
  bundle", how many `search` and `resolve` calls in that stretch returned no
  id the bundle served. A refused call follows nothing.
- *Conflict-flag precision* reads the agents' `next_action` events, each
  attributed to the latest bundle on its scope in its session before it:
  the flagged conflicts, the verdicts on them (a conflict judged twice keeps
  the later verdict), precision `real / (real + spurious)`, coverage (verdicts
  over flagged) and the verdicts on topics the bundle did not flag, which
  count in neither. *Next actions* prints the attributed actions, the share
  that `asked`, `proceeded` and `asserted`, and how many had no bundle before
  them.

`--json` prints the same report as one object: `since` (null for the
beginning), `until`, `scope` (empty for every scope), `time_to_ratification`
with `topics`, `ratified`, `unratified`, `unratified_share`,
`median_seconds`, `p90_seconds`, `ratified_on_arrival` and a `clocks` array
(`topic`, `scope`, `started`, `ratified`, `seconds`), and
`topic_operations` with `topics_opened` and `merges` and `splits`, each
`count`, `undone` and `per_topic`; `drill_down` with `bundles`,
`bundles_without_session`, a `sections` array (`section`, `served`,
`followed`, `share`) in a fixed order, and `looked_beyond` with `search` and
`resolve`, each `calls`, `beyond` and `share`; `conflict_flags` with
`flagged`, `verdicts`, `real`, `spurious`, `precision`, `coverage` and
`unflagged_verdicts`; and `next_actions` with `actions`, `asked`,
`proceeded` and `asserted` (each `count` and `share`) and `unattributed`. A
ratio with nothing to divide by is null.
Fields are added, never renamed.

`hearsay delete` takes exactly one selector and requires `--reason` even for a
preview. `--artifact` covers every revision. `--author` takes a configured
principal (with `--config`) or `source:native-id` (or `source:@handle`); a
principal expands to all configured source identities. The summary names the
covered L0 events, dependent L1 documents and L2 stances, topics, alias
candidates and pins, and the gestures in force the events made, which deleting
them takes out of force. `--json` prints the same walk as a JSON object. The
preview is the default and writes nothing.

`--apply` deletes, and needs `--config` and `--principal <human-id>`, a human
principal in that configuration. A missing flag, an unknown id or an agent is
refused before the database is opened, and `--principal` without `--apply` is
an error. One transaction records the deletion with the principal as operator,
redacts the covered L0 events in place, writes a `deletion` event under source
`hearsay`, takes out the pins the covered gestures made, and enqueues a
`distill` job for every L1 document the walk found
([ADR-0018](../../docs/adr/0018-operator-deletion-redacts-l0-in-place.md)).
It prints the deletion id, the events redacted and the documents queued, or
with `--json` the same as an object. Events an earlier deletion already covered
are left alone. `hearsay l0 get` on a deleted event fails naming the deletion
and operator, and prints no content.

`hearsay delete list` prints every deletion, newest first: id, time, operator,
status, selector and reason. `hearsay delete show <id>` prints one in full: the
events redacted, each queued document and whether it was re-distilled, deleted
or is still pending, the stances superseded whose position was redacted, and
the topics whose name was. Both take `--json` and no other flag but the
database's. The status is `complete` once every queued document is rebuilt and
no `distill` or `assert` job on what the rebuild touched is still to run, and
`rebuilding` until then.

`migrate`, `config`, `l0`, `aliases`, `topics`, `gestures` and `delete list|show` take an action word, and flags go on either side of
it and after its argument: `hearsay l0 get <id> --database-url x` and
`hearsay l0 --database-url x get <id>` are the same command. Each action reads
its own flags — `l0 list` and `l0 tail` take `--source`, `--kind` and
`--artifact`, `migrate down` takes `--i-know`, `topics split` takes `--stance` and `--name` — and a flag an action does not
read is an error rather than a no-op, the same way a stray argument is: a filter
silently dropped is a wrong answer nobody has a reason to doubt.

Every subcommand that runs something — the services, `all`, `config` and
`migrate` — takes `--log-level` and `--log-format`, which also read
`HEARSAY_LOG_LEVEL` and `HEARSAY_LOG_FORMAT`. `version` and `help` take no flags
and no arguments, and say so rather than ignoring what they were given. The
`instance` field on every log line is the hostname, or `HEARSAY_INSTANCE`.

`--database-url` points at Postgres, and also reads `HEARSAY_DATABASE_URL`,
which is the one to prefer: a URL on a command line puts its password in the
process list. It is on every subcommand that uses a database — `migrate`, `l0`, `aliases`, `topics`, `gestures`, `delete`, `eval`,
`all` and the four services — and every one of them refuses to start without
it.

`--listen` is on `connectors` and on `all`, which runs it, and also reads
`HEARSAY_LISTEN`. It is where the push connectors' handlers are mounted, under
`/hooks/<source>`, and where the connectors service answers `/healthz` and
`/readyz`
([ADR-0008](../../docs/adr/0008-observability-slog-and-opentelemetry.md)). The
default is `:8081`, the port after the API's 8080; a machine where something
else holds that port is why `all` takes the flag too.

`hearsay api` takes `--listen` as well, for its own address, and reads
`HEARSAY_API_LISTEN` rather than `HEARSAY_LISTEN`, because `all` runs both
services and one variable cannot name two ports. `all` spells it `--api-listen`.
The default is `:8080`. The API serves its calls as `POST /v1/<call>`, the same
calls as MCP tools at `/mcp`, and `/healthz` and `/readyz`.

`--config` points at the configuration repository, and also reads
`HEARSAY_CONFIG`. A service loads it before it starts and refuses to start if it
is invalid; started without one it warns and runs empty, which is what
evaluating the binary looks like. Configuration is read once
([ADR-0009](../../docs/adr/0009-configuration-as-a-gitops-directory.md)), so
this package has no reload path to maintain.

`hearsay distiller` serves `/healthz` and `/readyz` on `--listen` (default
`:8082`, environment `HEARSAY_DISTILLER_LISTEN`). `hearsay assert-worker`
serves the same paths on `--listen` (default `:8083`, environment
`HEARSAY_ASSERT_WORKER_LISTEN`). Both use the API's database and schema
readiness response. `hearsay all` runs these listeners too; move them with
`--distiller-listen` and `--assert-worker-listen`.

**Belongs here:** flag parsing, building config and dependencies, and calling
`Run`. Nothing else. A subcommand is a thin wrapper over
`internal/service/<name>.Run`; anything you would want to unit-test belongs one
level down, where it can be tested without a process.

The subcommand names are a contract with the Dagger module and the deployment
manifests. Renaming one is a breaking change, not a tidy-up.
