# Evaluating and qualifying the optional Jev boost

The boost ships without a qualified policy. Enabling it runs tasks in shadow
until an operator qualifies each task and explicitly installs its policy.
Confidence is a model score, not demonstrated correctness. Typed answers constrain
shape, not truth. This tooling makes no provider calls and supplies no default
quality limits. Automated tests use synthetic sources and recorded fake answers.

Use `hearsay eval judgments` to compare supplied worker predictions with adjudicated
labels, and `hearsay eval --config ... --json` for retained pipeline accounting.
The former runs offline without a database, configuration or credential. Producing
predictions on your own data requires the real workers; this command does not
reimplement their extraction, candidate retrieval or outcome reconciliation.

## Deliberate live measurement

1. Choose a representative, permission-scoped sample and a specific resolved model
   version. Include paraphrases, negation, suggestions versus decisions, edits,
   private/withheld positions, none-fit candidates and hostile instructions quoted
   in source text. Keep source revisions, document and position versions, candidate
   ids, effective ledger, question/schema version and access context in your
   provenance. Read the current vendor contract before the run. A model alias is
   unsuitable for qualification: all candidate predictions must report the exact
   version being qualified.
2. Have reviewers with access to the evidence adjudicate labels, resolve disagreements
   and record adjudicator identity, review revision and rationale. Create independent
   `tuning` and final `held_out` groups **before tuning**. Keep related revisions and
   paraphrases together. The validator refuses a group or source id in both splits;
   humans must also detect semantic relatives with different ids. Protect the held-out
   labels from the tuning operator. Use fresh held-out evidence after a failed run;
   repeated experiments on the same holdout compromise its independence.
3. Decide task-specific maximum error rates and minimum counts per gold class before
   opening held-out results. There are no universal limits. Decide whether empirical
   rates and these sample sizes are strong enough for the intended deployment; this
   implementation does not calculate confidence intervals or claim statistical
   certainty. Consider increasing counts, especially for rare decisions and errors.
4. In an isolated evaluation deployment with authorized source access, use your own
   configuration, database and credentials. Set the environment variable named by
   `llm.boost.api_key_env` (default `HEARSAY_JEV_API_KEY`) using your secret manager.
   Keep secrets out of JSON, configuration repositories and command arguments. Set
   the single `llm.boost.enabled` switch and explicit `model`, leave `policies` empty,
   then run the normal `hearsay distiller` and `hearsay assert-worker` processes (or
   `hearsay all` for development). Ingest the selected sources through the normal
   connectors and let the workers process them. This is deliberate provider spending;
   do not turn it into an automated test. Preserve the baseline outputs and committed
   shadow judgments, including fallbacks. Changing configuration requires restart.
5. Export/map the worker outputs and journal judgments to `predictions.json` by corpus
   example id. An operator's export harness reads the existing ACL-filtered
   `l1.Store.JudgmentsFor` API as an authorized reader and joins outputs at the captured
   revisions. There is no universal corpus-to-live-document mapping and this command
   supplies no replay runner or automatic label/export generator. A candidate prediction
   must express the **resulting domain label**, rather than an adapter option index.
   Baseline predictions must come from the actual current pipeline on the same
   evidence; do not fabricate missing outputs. For outcome classification, account for
   the worker's coherent label/text reconciliation through the generative path. Keep
   live inputs and reviewed corpora under the evidence's access and deletion controls.
6. Compare the supplied predictions, then qualify once with the limits you selected:

   ```sh
   hearsay eval judgments --corpus corpus.json --predictions predictions.json --split tuning --json
   hearsay eval judgments --corpus corpus.json --predictions predictions.json --qualify operator-options.json --json > qualification.json
   hearsay eval --config ./evaluation-config --since 2026-10-01T00:00:00Z --until 2026-10-08T00:00:00Z --json > pipeline-report.json
   ```

   Comparison defaults to `--split held_out`; `--split all` is also available.
   Qualification always uses both splits independently, regardless of comparison.
   Failed qualification and insufficient evidence emit the measured JSON and return
   a nonzero exit status; malformed/missing inputs are refused before output.
7. Only `status: qualified` includes a `policies` object. Copy that object below
   `llm.boost.policies`, retaining any independently qualified task policies, and run
   `hearsay config validate ./evaluation-config` before restarting. The emitted entry
   has `name`, `model_version` and `threshold`, matching the configuration schema.
   Installing a policy is an explicit operator action. Requalify with new independent
   evidence whenever the model or task/question variant changes. This change ships
   no deployment policy, and seed fixtures are never deployment evidence.

## Corpus and prediction files

All files are strict JSON: unknown fields and trailing values are refused.
`internal/eval/testdata/seed-corpus.json` is a complete reviewable format example.
Its sources, labels and recorded answers are synthetic teaching fixtures, authored
for tests, not human-adjudicated team evidence or live measurements. The seed covers
all four task variants on both splits and all six required adversarial categories.

A corpus has `version: 1`, a `provenance` description and `examples`. Every example
has a unique `id`, a related-evidence `group`, `task`, `split`, `category`, `label`,
`sources` and `review`. A source requires `id`, immutable `revision`, `uri`, nonempty
`acl` and the minimized `text` the reviewer saw. The review requires `adjudicator`,
`revision` and `rationale`. Add exact worker input identities/question versions to
`provenance` or the rationale, or refer to an immutable retained manifest there.
Corpus text is input-only: reports contain ids, labels, counts and numbers.

Labels are `changes`/`restates` for `stance_comparison`; `resolved`, `decided`,
`proposed`, `open`, `none` for `outcome_classification`; and an offered candidate id
or `none_fit` for `topic_matching` and `topic_shortlist`. Topic examples also supply
`candidates` (ids, never topic names). A prediction may use `new` to represent a
baseline topic creation, or blank/`abstain` for an unusable candidate answer.

`predictions.json` contains `baseline` and `jev` arrays. Each entry has `example_id`,
`model_version`, `label`, and nullable `confidence`. Qualification requires complete
predictions for its task, a usable baseline label, exact candidate model match and
known confidence for non-abstaining answers. Other tasks may be supplied in the same
file. Optional `conflict_flag`, `conflict_real` and `corrected` booleans supply
reviewed downstream outcomes; omit them when unknown. Do not infer them from model
confidence. In comparisons, missing predictions are counted separately and excluded
from rate denominators; qualification refuses missing predictions.

Reports compare baseline and Jev separately per task:

| Measure | Numerator / denominator |
|---|---|
| `errors` | wrong labels / supplied predictions |
| `false_changes` | predicted changes on gold restates / gold restates |
| `missed_changes` | gold changes predicted otherwise (including abstain) / gold changes |
| `incorrect_attachments` | wrong existing candidate attachments / predicted existing attachments |
| `duplicate_topics` | predicted new on a gold existing candidate / gold existing candidates |
| `outcome_confusion` | wrong outcome labels / outcome examples |
| `missed_decisions` | gold decided or resolved predicted neither / gold decided or resolved |
| `conflict_precision` | real flagged conflicts / flagged conflicts with a verdict |
| `corrections` | corrected outputs / outputs with a correction review |

The outcome confusion matrix has all five gold rows and five predicted columns,
plus an abstain column. Counts include zeros. No denominator means a null rate.
A decided/resolved substitution is outcome confusion but not a missed decision.
Choosing none-fit does not itself mean a duplicate topic; only explicit baseline
creation (`new`) can count as one. A baseline `new` on gold `none_fit` counts as
correct for matching accuracy: both mean no existing attachment. This says nothing
about whether the generative topic wording is useful.

## Qualification method and operator options

`operator-options.json` requires `task`, `model_version`, `name`, `limits` and
`minimum_per_class`. Supply your own values; there are no defaults. `limits` maps
one or more relevant metric names to maximum empirical rates in `[0,1]`:

| Task | Permitted limit names | Required evidence classes |
|---|---|---|
| stance comparison | `false_changes`, `missed_changes`, `errors`, `corrections` | `changes`, `restates` |
| topic matching and shortlist (separate policies) | `incorrect_attachments`, `duplicate_topics`, `errors`, `corrections` | `existing`, `none_fit` |
| outcome classification | `outcome_confusion`, `missed_decisions`, `errors`, `corrections` | `resolved`, `decided`, `proposed`, `open`, `none` |

`minimum_per_class` maps **each** required gold class to an operator-selected
positive integer. It applies both to held-out examples and to adopted candidate
judgments in that class. The exception is `none_fit`, which is never adopted and
needs only total held-out examples. This prevents qualification based entirely on
baseline fallback. Unmeasurable requested limits (for example corrections without
reviewed correction outcomes) yield insufficient evidence.

The threshold search uses only tuning predictions and labels: candidates are 0, 1,
each tuning confidence and its next representable higher value (at most 1).
At each threshold, below-threshold, abstaining and topic none-fit/new predictions
fall back to their **entire baseline prediction**, including downstream verdicts.
Among thresholds satisfying tuning limits, minimize total effective-label errors,
then maximize adoption count, then prefer the higher threshold. If none satisfies
tuning limits, choose the least-error threshold by those same tie breakers. Tuning
can never grant acceptance: the fixed candidate still needs the independent
held-out gate. No threshold search or retry is performed on held-out labels.

The selected threshold is evaluated once over held-out effective outputs. The JSON
contains threshold, tuning and held-out metrics, operator limits, total and adopted
class counts, failed limit names and insufficient evidence reasons:

- `insufficient_evidence`: a class/adopted count is below its minimum or a requested
  metric has no denominator. Measured failures are still shown; no policy is emitted.
- `failed_qualification`: enough evidence exists, but one or more named limits fail.
- `qualified`: every limit and evidence requirement passes; only then is a
  configuration-compatible policy emitted.

Missing held-out labels, limits, evidence requirements, baseline predictions or
candidate predictions are refused. Any candidate model mismatch is refused,
including on the tuning split. Confidence is never an access-control boundary.

## Pipeline accounting and interpretation

`hearsay eval --json` retains every existing field and adds `pipelines` with
`baseline`, `shadow` and `accepted` cohorts. These count retained **worker-stage
records**, not documents or replay invocations. The cohort records the worker's
final stage classification, rather than simply whether a policy was configured.
For example, an assertion run restored after budget exhaustion is baseline; inspect
each judgment's mode and policy separately. `unclassified_runs` makes unexpected
cohort labels visible.
Deletion removes observations referencing deleted evidence, so this is retained
accounting, not a provider invoice or permanent billing ledger.

Each cohort reports runs; total, mean and p90 handler seconds; total queue delay
seconds, mean known queue delay and missing-delay run count; logical generative calls,
provider attempts (including retries) and usage; logical judgment requests,
attempts, fallbacks and usage; additional candidate count; total reported USD cost; and fallback frequency. Handler duration already
includes optional work, retries, waits and candidate processing: never add those
again. Queue delay is separate. The p90 is nearest-rank over complete handler
measurements; unknown handler runs make its p90 and mean unknown. Queue mean covers
only known delays and must be read with `missing_queue_delay_runs`.

A logical judgment request is one question resolved in the stage run, including
abstention, zero-attempt cooldown/budget suppression and local oversize refusal.
Retries add provider attempts, never logical requests. A committed journal result
reused in a later run is not counted again: its original run counted it. Fallback
frequency is fallbacks / logical requests, **not** fallbacks / provider attempts.
One rate-limit attempt followed by two cooldown suppressions is three fallback
observations and one provider attempt. Suppressions consume known zero tokens and
cost; an attempted call with unreported usage remains unknown. Both workers populate
the logical-request count, including explicit zero when nothing was asked.

Generative accounting is scoped to each stage context, including optional
restatement and shortlist re-extraction. Provider attempts include failed and
retried attempts; reported tokens and costs from each attempt contribute. An
uninstrumented custom Completer leaves attempts unknown. Legacy zero token fields
without presence bits stay unknown; adapters with presence bits retain reported
zero. Concurrent jobs never difference a shared registry usage counter.

Empty cohorts have null measures. Nullable consumption is summed only when every
contributor is known, nonnegative and within representable bounds; missing/invalid
values and integer or floating-point overflow yield null, never zero, saturation,
wraparound or infinity. Generation and judgment consumption both contribute to
total cost, including incurred retries and additional processing. Costs come only
from provider-reported consumption, never from a price table. A missing generative
cost makes pipeline cost unknown even if Jev cost is reported. No savings claim
follows from Jev's price, fixture timings, or unequal workloads: compare matched
inputs, complete pipeline accounting and measured quality before drawing conclusions.
