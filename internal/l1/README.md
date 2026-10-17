# internal/l1

The document layer: what was said. Artifacts and fixed channel conversations
have a document; long chat conversations may also have gated burst documents,
markdown documents have independently distilled wiki sections, and transcripts
have one independently distilled segment per topic.
All are flat rows in one schema, rebuilt from L0 when their events change.

**Belongs here:** the envelope and the per-kind bodies, the store, deterministic
reference extraction, the secrets and PII scrub that runs before `text` is
written, and hybrid search over the table (embeddings plus full text).

**Does not belong here:** the prompts that produce a document — those belong to
the distiller in `internal/service/distiller`, which owns its own prompt
(ADR-0005). Entities, topics and stances are `internal/l2`.

Distillation is a write-time LLM call. The only model call a read makes is the
one that turns a search query into a vector, on the `embed` tier; nothing on the
read path asks a model to generate anything.

## Things to know before changing it

- **A document is a function of its events.** [Build] takes the current revision
  of an artifact and of everything that hangs off it; [BuildChatWindow] takes
  the current messages in a fixed channel time bucket; [BuildChatReplies] takes
  a channel message and the replies whose `thread` names it, on a source such
  as Slack where a reply's thread is the message it answers; [BuildChatBursts] derives
  gated single-author runs from those current messages; [BuildWikiSections]
  partitions markdown at headings; [BuildMeetingSegments] takes numbered
  transcript ranges and combines repeated ranges for one topic. They produce everything but
  the body; the body is the one part that takes a model call. Nothing else may
  reach into a document — not a clock, not a map iteration order — or
  re-distilling would rewrite a row that has not changed, which is the property
  the acceptance test pins.
- **Only current revisions.** An artifact's earlier revisions are in L0 and are
  what a provenance walk reads. An access-list re-sync is a revision like any
  other, so building from anything but the newest would inherit an access list
  that has been replaced. An unchanged wiki section or meeting segment may retain its prior L0
  reference when another part changes; an ACL change refreshes all parts.
- **The access list is folded, not inherited.** A document quotes every event in
  it, so it carries the grants every one of them carries. A reply that shares no
  grant with the artifact is left out of the document rather than narrowing it to
  nothing: a document with an empty access list is unreadable for ever, which is
  not the same thing as a private one.
- **Nothing unresolved is invented.** An @-mention or an author that the identity
  mapping does not cover produces no participant and no reference; the resolver
  keeps the sighting for a person to map (`internal/principal`). Fixing the
  mapping and re-distilling is what recovers authorship.
- **`text` is embedded, `raw_text` is not.** `text` is the distillation — the
  model's words, nothing else — because a vector over one document about one
  thing is what a similarity search is for. `raw_text` is the team's own words,
  for the full-text index. A vector is a function of the text it was made from,
  so [Store.Put] clears the embedding whenever it writes different `text`: what
  needs embedding is what has no vector, which is also every document in a
  deployment that has configured no `embed` tier.
- **The scrub runs over both, over the body, and over a `url` reference.** Those
  are the four strings a row holds that a person wrote; the other reference ids
  are structured, and a marker in one would break the join it exists for. It is
  idempotent by construction: every replacement is a marker no pattern matches
  the inside of, and there is a test that says so. It is a net rather than a
  proof, and it is the last of the four access-control control points, not the
  first (docs/design.md#access-control).
- **A reference is never read out of the inside of a link.** A URL contains
  every shape extraction looks for — `#31` in a path, `@name` in a userinfo or a
  path, an entity name in a directory — so links are matched first and the rest
  of the text is read with them blanked out. An `@`-mention also has to start a
  word, or it fires on the domain of an email address.
- **A change's paths are matched, not kept.** A pull request's L0 event
  carries the paths it touches (bounded by the contract); [References] links
  the document to every `code/` entity in that repository whose path patterns
  match one, most specific first, and nothing downstream sees the file list.
  Only configured entities with a `repo:` are matched.
- **`outcome_kind` is the L2 trigger and is on every document.** The five values
  are the design's, the column carries a CHECK over exactly them, and
  `OutcomeKind.Asserts` is the one place that says which of them the assertion
  pipeline reads.
- **The column is `refs`, not `references`,** because `references` is reserved in
  SQL — the same reason L0's `time` is `occurred_at`.
- **Two grants are the same grant when the kind, the source and the native id
  agree.** The label is what the source calls a grant so that a person reading
  an access list knows what it is, and a document is not less readable for
  having been labelled differently — so the label is never compared, here or in
  [Build]'s fold. It is why the ACL filter is a jsonb containment and why the
  index for it is `jsonb_path_ops`: containment ignores an entry's extra keys.
- **An access-list entry's `native_id` carries no namespace, and the mapping has
  two.** A principal's native id in a source and their handle are separate keys
  that may legitimately hold the same string for different people
  (`internal/principal`), while an entry is one string the source wrote down. So
  a configured native id is a grant as it stands, and a handle is a grant only
  where nobody else answers to that spelling in either namespace
  ([principal.Resolver.Claims]). A spelling two principals answer to grants
  neither of them anything: refusing it costs somebody a document they may read,
  and taking it would hand them one they may not.
- **Search filters before it ranks.** Both halves — similarity over the
  embedding, Postgres full text over `raw_text` — rank the documents the caller
  may read, and the two rankings are fused by reciprocal rank. Filtering
  afterwards would let a document the caller may not read push one they may out
  of the candidates, which is a leak nothing downstream can see
  (docs/design.md#access-control). [ReaderFor] is the one place a caller's
  grants are derived from the identity mapping, so two callers cannot disagree
  about what somebody may read.
- **The text search configuration is named, every time.** `english`, in the
  index and in every query. `default_text_search_config` is a session setting
  nothing here pins, and an index built under one and queried under another
  matches nothing at all.
- **There is deliberately no ANN index on the embedding.** An approximate index
  ranks first and filters afterwards, which is the wrong way round for a read
  that has to filter first, so search does an exact distance over what the
  filter leaves. Adding one means deciding what happens to the filter, not just
  writing a `CREATE INDEX`.

See [docs/design.md](../../docs/design.md#l1-distilled-documents).

## Judgment journal and pipeline accounting

The journal is an optional write-path record, with no model calls in this
package. `JudgmentInputSpec` names the incoming document, ordered candidate
ids, exact compared stance ids, source event ids (also needed before the first
L1 write), scope and computed ACL. `CaptureJudgmentInputs` records their
identities and revisions, including `distilled_at`, hashes of the rows and
all candidate/position evidence, and the source and effective-ledger versions.
Candidate dependencies include ledger-connected topic rows and their evidence,
including virtual split topics and merged-away rows. This closure is
conservative across undone operations; it does not replace L2 candidate
retrieval or its permission checks.
It returns no source, position, prompt or answer text. `DocumentRevision`
lets a worker compare the captured revision to the document it actually read.
Capture **before** reading the state used in a question, while holding the
worker's existing scope key where applicable. If a later read sees another
revision, discard the optional judgment or capture and judge again.

`BeginJudgment` on an autocommit store records an attempt before the provider
call. Use its returned id for the final `JudgmentRecord`. An attempt remains
an attempt through a crash or refused commit; only a `committed` record may
be reused. `LookupJudgment` checks the current state and exact question/hash
identity in the same SQL snapshot. A requested model version is deliberately
absent from the lookup key: reuse returns the original resolved version,
policy, label and accounting without reinterpreting the result. A committed
record is immutable, and committing that exact same record again is safe.
Question/schema versions, task, policy and result labels belong to the worker;
only categorical labels and hashes belong in these fields, never question or
completion text.

Call `CommitJudgments` in the worker's **READ COMMITTED** baseline transaction,
**before** writing documents, topics or stances. It validates every judgment
and observation before writing any of them, then records the batch under a
savepoint. `JournalStorageFailure`, `JournalTimeout` and
`JournalInputsChanged` are distinct sanitized refusal values, compared with
`errors.Is`. The savepoint rolls back the whole optional batch on a refusal,
including an earlier successful record or observation in that batch, and
restores the caller's server timeout settings. The caller then writes its
complete baseline result. The journal never writes a consumer's result.
Use the live baseline context here, not the optional model call's exhausted
deadline; connection loss or cancellation of the baseline transaction itself
cannot be repaired by a savepoint. Repeatable-read/serializable transactions
are refused rather than validating against an old snapshot.

Validation takes `SHARE ROW EXCLUSIVE` on `judgment_input_guard` until the
**outer transaction** commits. Effective-input mutation triggers take
`ROW EXCLUSIVE` before writing, so another writer cannot change inputs in
the interval between validation and commit. Successful journal commits
serialize this short database interval, including the caller's consumer
writes, but never provider work. Server statement and lock timeouts default
to 100 ms each and are configurable per call. A refused savepoint releases
its guard immediately so the baseline write can proceed. Source writers
remain concurrent: source/ledger change markers are appended, not updated
on a single counter row. Counts of visible markers include late commits
that a sequence watermark would miss. Source and ledger changes invalidate
reuse conservatively across scopes, including unrelated changes. This may
cost a fresh judgment; it cannot authorize stale reuse. Do not prune the
marker log independently of the journal. **Any new table that changes
effective inputs must join this trigger/guard protocol.**

`PipelineObservation` is one stage run for `baseline`, `shadow` or `accepted`.
It includes handler/queue time in nanoseconds, generative calls/tokens/cost,
judgment attempts/fallbacks/tokens/cost and candidate processing. Durations,
counts and usage are pointers: nil means unknown, and a pointer to zero is
reported zero. `AddPipelineCount` and `llm.AddJudgmentUsage` propagate unknown
and overflow. Negative/non-finite accounting is cleared to unknown before
storage. Observations use the same commit operation, including baseline runs
with no judgment records; their evidence snapshots are validated too.
Summaries and reports over these tables belong to `internal/eval`.

`JudgmentsFor`, `LookupJudgment` and `ObservationsFor` filter both scope and
stored ACL, then check current evidence access, including the newest visible
source revisions while L1 is waiting for its ACL rebuild. An operator deletion removes
attempts, committed records and observations referencing any covered source
or document, including candidate/compared-position evidence. The deletion
trigger lives on `l0_deletions`. The operator service takes its input guard
before establishing its repeatable-read snapshot, so it also removes journal
rows committed while deletion was starting. Direct `l0.Delete` callers with
a repeatable-read transaction must take that guard before their first read
too. Insert guards prevent recreating a reference after deletion.

Distillers can set `JudgmentInputSpec.SourceBuilt` when the incoming document
is built from the complete `EventIDs` source revisions and the captured ACL and
scope. The output document's own L1 row is then excluded from the input state,
while its identity remains in deletion provenance and current ACL checks. This
allows writing that derived row and reusing the committed judgment on restart.
Source hashes/revisions, global source/ledger change markers, ACL/scope identity
and deletion validation still apply. The default remains a full L1 input
snapshot for assertion consumers. Do not use source-built identity when the
question depends on previously generated L1 text.
