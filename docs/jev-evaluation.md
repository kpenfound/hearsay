# Measuring Jev

The distiller and the assertion worker decide from Jev's answers to atomic
yes/no questions: Jev makes judgments, code makes decisions. Only a confident
yes counts, a yes at or above `llm.judgments.threshold` (default 0.8); every
other answer is a no.

| Task | The question | What code decides from it |
|---|---|---|
| `stance.same_answer` | Does a new position give the same answer to a topic as its current position? | A confident yes restates; anything else changes. |
| `topic.same_question` | Does an assertion answer the same question as one candidate topic? Asked once per candidate. | The most probable confident yes is the topic the assertion continues; with none it opens a new topic. |
| `outcome.work_finished` | Does the document record a question answered or work finished? | Resolved. |
| `outcome.binding_agreement` | Does it record an agreement later work must follow? | Decided. |
| `outcome.unagreed_proposal` | Does it put forward something nobody has agreed to? | Proposed. |
| `outcome.still_open` | Does it say the matter is still being worked out? | Open. |

The outcome is the first confident yes in that order, and none without one.
Confidence is a model score, not demonstrated correctness, and typed answers
constrain shape, not truth. Measuring Jev on a team's own sources is how to
know how far to trust it, and how to choose the threshold.

`hearsay eval judgments` compares supplied predictions with adjudicated labels,
offline, without a database, configuration or credential. `hearsay eval
--config ... --json` reports retained pipeline accounting. Neither makes a
provider call; automated tests use synthetic sources and recorded answers.

## Measuring on your own sources

1. Choose a representative, permission-scoped sample. Include paraphrases,
   negation, suggestions versus decisions, edits, private or withheld positions,
   none-fit candidates and hostile instructions quoted in source text. Keep
   source revisions, candidate ids, the question version and the resolved model
   version in your provenance.
2. Have reviewers with access to the evidence adjudicate labels, resolve
   disagreements and record adjudicator identity, review revision and
   rationale. Create independent `tuning` and `held_out` groups before looking
   at results, keeping related revisions and paraphrases together; the
   validator refuses a group or source id in both splits.
3. Run the normal workers against those sources in an isolated deployment with
   authorized source access and your own credentials. This is deliberate
   provider spending; do not make it an automated test.
4. Export each judgment from the journal (`l1.Store.JudgmentsFor`, as an
   authorized reader) to `predictions.json` by corpus example id, as its `yes`
   or `no` and its confidence. Do not fabricate missing outputs, and keep live
   inputs and reviewed corpora under the evidence's access and deletion
   controls.
5. Compare:

   ```sh
   hearsay eval judgments --corpus corpus.json --predictions predictions.json --split tuning --json
   hearsay eval judgments --corpus corpus.json --predictions predictions.json --json
   hearsay eval --config ./config --since 2026-10-01T00:00:00Z --until 2026-10-08T00:00:00Z --json > pipeline-report.json
   ```

   Comparison defaults to `--split held_out`; `--split tuning` and `--split all`
   are also available.
6. Choose the threshold from the tuning split, reading the confidences of the
   false yeses and false noes there, then check it once on the held-out split.
   Set it as `llm.judgments.threshold` and restart the two workers. Measure
   again with fresh evidence when the model changes.

## Corpus and prediction files

All files are strict JSON: unknown fields and trailing values are refused.
`internal/eval/testdata/seed-corpus.json` is a complete reviewable format example.
Its sources, labels and recorded answers are synthetic teaching fixtures, authored
for tests, not human-adjudicated team evidence or live measurements. The seed covers
every task on both splits and all six adversarial categories.

A corpus has `version: 1`, a `provenance` description and `examples`. Every example
has a unique `id`, a related-evidence `group`, `task`, `split`, `category`, `label`,
`sources` and `review`. The label is `yes` or `no`: every task is one yes/no
question. A source requires `id`, immutable `revision`, `uri`, nonempty `acl` and
the minimized `text` the reviewer saw. The review requires `adjudicator`, `revision`
and `rationale`. Add exact worker input identities/question versions to
`provenance` or the rationale, or refer to an immutable retained manifest there.
Corpus text is input-only: reports contain ids, labels, counts and numbers.

A per-candidate judgment is one example per candidate. To label whether an
assertion continues one of three candidate topics, write three
`topic.same_question` examples, one yes at most, in the same `group`. An outcome is
four examples, one per outcome judgment.

`predictions.json` contains `jev` and `baseline` arrays. Each entry has `example_id`,
`model_version`, `label` (`yes`, `no`, or blank for no answer) and nullable
`confidence`. `baseline` is any reference predictor to compare Jev with, such
as a generative model's implied answer, and may be empty. Optional
`conflict_flag`, `conflict_real` and `corrected` booleans supply reviewed downstream
outcomes; omit them when unknown. Do not infer them from model confidence. In
comparisons, missing predictions are counted separately and excluded from rate
denominators.

Reports measure Jev, and the reference predictor, separately per task:

| Measure | Numerator / denominator |
|---|---|
| `errors` | wrong or unanswered labels / supplied predictions |
| `false_yes` | predicted yes on gold no / gold no |
| `false_no` | gold yes predicted no or unanswered / gold yes |
| `conflict_precision` | real flagged conflicts / flagged conflicts with a verdict |
| `corrections` | corrected outputs / outputs with a correction review |

`unanswered` counts blank predictions. The confusion matrix has gold `yes` and `no`
rows and predicted `yes`, `no` and `none` columns, with zeros included. No
denominator means a null rate.

What an error costs depends on the judgment:

| Task | A false yes is | A false no is |
|---|---|---|
| `stance.same_answer` | a change served as a restatement | a false change |
| `topic.same_question` | an incorrect attachment, one `hearsay topics split` to undo | a duplicate topic, one `hearsay topics merge` to undo |
| `outcome.binding_agreement` | an invented decision | a missed decision |
| the other outcome judgments | a wrong outcome kind | a wrong outcome kind |

## Pipeline accounting

`hearsay eval --json` reports `pipelines` with a `distill` and an `assert`
stage. These count retained worker-stage records, not documents or replay
invocations; `unclassified_runs` makes an unexpected stage visible. Deletion
removes observations referencing deleted evidence, so this is retained
accounting, not a provider invoice.

Each stage reports runs; total, mean and p90 handler seconds; total queue
delay seconds, mean known queue delay and missing-delay run count; logical
generative calls, provider attempts (including retries) and usage; logical Jev
requests, attempts and usage; candidate count; and total reported USD cost.
Handler duration already includes judgments, retries and waits: never add them
again. The p90 is nearest-rank over complete handler measurements; unknown
handler runs make its p90 and mean unknown. Queue mean covers only known delays
and must be read with `missing_queue_delay_runs`.

A logical Jev request is one batch of questions sent together (every question
of a batch shares one state). Each question has its own journal record; a
batch's attempts and usage are recorded on its first new record and as known
zero on the others. Retries add provider attempts, never logical requests. A
journaled answer reused in a later run is not counted again. A job whose Jev
call failed recorded nothing; the queue's retry is a new run.

Nullable consumption is summed only when every contributor is known,
nonnegative and within representable bounds; missing or invalid values and
overflow yield null, never zero. Generation and judgment consumption both
contribute to total cost, from provider-reported consumption only, never a
price table. No savings claim follows from Jev's price or unequal workloads:
compare matched inputs, complete accounting and measured quality.
