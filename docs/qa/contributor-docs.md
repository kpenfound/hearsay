# Contributor and reference audit — 2026-11-04

This audit covers CONTRIBUTING.md, AGENTS.md, the design, configuration,
connector contract and judgment evaluation references, the ADR index, the command
README and all 20 READMEs under `internal/` (28 documents total). Code is the
reference. This change corrects descriptions, not connector contracts or runtime
behavior. Existing trial records and accepted ADR bodies are unchanged.

## Corrections and reported divergences

The contributor guide now names AGENTS.md, all four direct dependencies in
`go.mod`, the eight registered connector types and the shipped HTTP/MCP search
call. AGENTS.md includes identities, aliases and offline judgment evaluation.
The command table includes help and distinguishes database-backed evaluation
from offline evaluation. Design status describes the implementation, and the
read-path prose accounts for search's optional query embedding. No embedding
adapter ships in `providers.All`, so this binary currently uses full text alone.

The Dagger check table includes the `go-module` collection dimension seen on the
pinned module. Both migration Secret examples use `env://` syntax. SDK guidance
now distinguishes SDK v1.0.1, the module scope's engine beta.11 declaration and
the workflow's beta.25 CLI, without asserting an unsupported version comparison.
Slack history and ACL re-sync, work source configuration, hierarchy precedence,
segmentation, stance version identity, authored fixture recording, renewed leases,
service README placement and build-info fallback descriptions were refreshed.
The queue usage example's closing code fence was repaired.
The ADR index lists all 31 records, including ADR-0031.

The following discrepancies were reported through factory discoveries and retained
for separate follow-up; this change does not fix their code or pins:

- The bare `github.com/dagger/go` lock entry selects v0.4.0 at
  `e327bee9103fe60cb66c9d11bf2e1604686021df`, contrary to the prior v0.3.1
  contributor/runner guidance. v0.3.1 helper resolutions remain. Retained `main`
  and `refs/heads/main` entries also select e327bee9, rather than the runner's
  accepted 5ec2ba85 exception. The lock retains `golang:1.26-alpine` alongside
  1.27.1 images and the SDK's 1.25-alpine, contrary to the previous cleanup rule.
  The lock was neither regenerated nor hand-pruned.
- By code reading, Discord permission tokens hash only sorted channel overwrites.
  With unchanged edit timestamps, a return to identical overwrites reuses an old
  event id. L0 deduplicates without advancing its sequence: public/private/public
  can leave restricted access current, while private/public/private can leave
  PUBLIC access current. Re-sync emits the same deduplicated id. Guild role
  permissions also affect ACLs but are absent from the token; a role-only change
  can produce `ErrRewrite`. These are reported implementation defects, not new
  requirements for third-party connectors. No live Discord reproduction was run.

## Sample traces for every audited document

Paths below are relative to the repository root. Unchanged documents were audited
as well; a matching statement did not need an edit.

| Document | Sample statement | Supporting implementation |
| --- | --- | --- |
| `CONTRIBUTING.md` | Four direct dependencies; explicit source registry | `go.mod`; `cmd/hearsay/run.go:connectorRegistry` |
| `AGENTS.md` | Four services plus identities, aliases and evaluation commands | `cmd/hearsay/run.go:commands` |
| `docs/design.md` | Search embeds a query when an embedder is supplied | `internal/l1/search.go:embedQuery`; `internal/service/api/calls.go:search` |
| `docs/config.md` | Work posters use principal tokens; Slack backfill persists a cursor | `internal/connector/work/work.go`; `internal/connector/slack/backfill.go`; `internal/connector/runtime.go:backfill` |
| `docs/connector-contract.md` | Required Connector methods and optional ingest interfaces; Discord token limitation | `internal/connector/connector.go`; `internal/connector/discord/events.go:permissionToken` |
| `docs/jev-evaluation.md` | Qualification refuses nondefault comparison splits | `cmd/hearsay/eval_judgments.go:runJudgmentEval` |
| `docs/adr/README.md` | All 31 ADRs indexed with status and date | `docs/adr/[0-9]*.md` headers; body hashes unchanged |
| `cmd/hearsay/README.md` | Help lists the command surface; offline evaluation needs no DB | `cmd/hearsay/run.go:commands`; `cmd/hearsay/eval_judgments.go` |
| `internal/bundle/README.md` | Directive comes from L0 and cannot be trimmed | `internal/bundle/bundle.go` |
| `internal/config/README.md` | Absent and empty authority lists differ | `internal/config/schema.go`; `internal/config/validate.go` |
| `internal/connector/README.md` | Runtime resolves secrets and builds a gate per source | `internal/connector/runtime.go:NewRuntime`, `ResolveSecrets` |
| `internal/db/README.md` | Connect checks the schema; development all migrates itself | `internal/db/db.go:Connect`; `cmd/hearsay/run.go:runAll` |
| `internal/db/migrations/README.md` | Migration names/annotations are checked; migration 1 creates vector | `internal/db/migrations_test.go`; `internal/db/migrations/00001_create_vector_extension.sql` |
| `internal/eval/README.md` | JSON adds pipeline accounting and offline comparison | `internal/eval/eval.go:Report`; `judgment_metrics.go`; `judgment_corpus.go` |
| `internal/l0/README.md` | Duplicate event IDs do not rewrite rows; operator deletion redacts | `internal/l0/store.go:Append`; `internal/l0/deletions.go` |
| `internal/l1/README.md` | Hybrid search filters inside both rankings | `internal/l1/search.go:Search` |
| `internal/l1/testdata/state-qa/README.md` | Trial checks the QA transcript status | `internal/l1/testdata/state-qa/run.py` (historical run not repeated here) |
| `internal/l2/README.md` | Topic operations are an append-only ledger | `internal/l2/operation.go`; `internal/l2/effective.go` |
| `internal/l3/README.md` | Recent activity applies reader access before limiting | `internal/l3/views.go:Recent`; `internal/l1/readable.go:ListFor` |
| `internal/llm/README.md` | Worker renews its lease; model callers still need deadlines | `internal/queue/worker.go:keepLease`; worker `CallTimeout` constants |
| `internal/onboard/README.md` | Slack identities require unique confirmed email matches | `internal/onboard/people.go`; `internal/connector/slack/reader.go` |
| `internal/principal/README.md` | Matching is source-scoped; conflicting keys remain ambiguous | `internal/principal/resolver.go` |
| `internal/queue/README.md` | A due debounced job stays due | `internal/queue/queue.go:Enqueue` conflict update using `clock_timestamp()` |
| `internal/service/README.md` | RunAll cancels on first return and joins errors | `internal/service/service.go:RunAll` |
| `internal/service/distiller/README.md` | Outcome optional work has a five-second budget | `internal/service/distiller/outcome.go:OutcomeWorkBudget` |
| `internal/service/api/testdata/work/README.md` | Authored work exchanges load and fail on an unrecorded request | `internal/service/api/work_integration_test.go:workRegistry` |
| `internal/telemetry/README.md` | Logger follows context and emits to stderr in command wiring | `internal/telemetry/logger.go`; `cmd/hearsay/run.go:withLogger` |
| `internal/version/README.md` | Unstamped file-only builds fall back to devel/unknown | `internal/version/version.go:Info` |

## Validation

Direct execution used Go 1.27.1 on Linux arm64. `go test ./...`, `go vet ./...`
and a binary build passed. After the example edits, fresh
`go test -count=1 ./cmd/hearsay ./internal/config ./internal/connector ./internal/eval`
passed, including the tests that load the configuration and connector examples
and inspect the command README. Direct `go test -race ./...` was refused because
the host has CGO disabled and no C compiler; it is not counted as a race pass.

The built binary's `help`, `init --help` and `eval judgments --help` were checked.
Offline judgment comparison with the synthetic seed corpus/predictions and
`--split tuning --json` passed; qualification with `--split tuning` was refused
with `qualification controls its own tuning and held-out splits`, as documented.
No live source or model provider was contacted.

Dagger CLI v1.0.0-beta.25 discovered no checks in the factory's boundary-less
view. A byte-copied temporary workspace with an empty `.git` discovery directory
loaded the actual modules; no version-control operation was used. `dagger check
-l` listed the nine documented checks and `dagger check -l --all --go` expanded
Go module, package and test dimensions. The source/config declarations were
cross-checked against that listing, not inferred from the initial empty result.

The migration example was invoked with a disposable dummy environment URL and
`--action=doc-audit-invalid`. Dagger resolved
`Workspace.resolve(value: "env://HEARSAY_DATABASE_URL").secret` as `Secret!`,
loaded/type-checked `hearsay.migrate`, built the native image and reached the
binary's intentional `unknown action` refusal. This confirms the corrected
provider form without touching a real database; it is not a migration success.

A one-off link/anchor checker found all 184 relative links and anchors across
these 28 documents resolve. The operator unit owns the complete end-to-end link
walk and deployment rehearsal. SHA-256 comparisons verify the root lockfile and
every existing ADR body are byte-identical to their pre-audit snapshots.
The unchanged root lockfile SHA-256 is
`13be864d51af0f46435f608d5bba31a5632682fbb0729f6505b3a608787abd56`.

The full configured `dagger check` run evaluated 571 results: 568 passed and
three failed. Passing checks included lint, tidy, Helm, SDK and Go staleness,
and both-platform image checks. The failures and targeted reruns were:

- `compose-check` initially failed because the factory's staged configuration
  directory was mode 0700 and its YAML file mode 0600, unreadable to image UID
  10001. Adding read/traverse bits only in the temporary copied workspace made
  the isolated check pass. Configuration content and repository modes were
  unchanged; this is a pass in the normalized copy.
- The Go check `TestAdapterRetryRecoveryAndDeadlines/attempt_timeout` initially
  expected two attempts but observed three after 104ms. Its isolated check
  passed, including all five subcases.
- `hearsay:integration-test` failed initially on
  `TestBoostJobBudgetReleasesScopeToQueuedWriteAndHuman`. An isolated rerun,
  after fresh migrations with `go test -race -tags=integration ./...`, still
  failed. It also reported the three additional failures below. No code or
  test changes were made, and these failures are not classified as proven
  flakes.

The isolated integration failure details were:

```text
TestBoostJobBudgetReleasesScopeToQueuedWriteAndHuman
  boost_budget_integration_test.go:133: scope held too long:
  baseline=56.525ms budget=1s job=1.242092s human=1.270557s later=1.245909s
TestBoostRunningDistillAssert/failure
  boost_integration_test.go:360: missing end-to-end judgments: 4
TestWatchResumesFromItsCursorWithoutDuplicatesOrGaps
  watch_integration_test.go:339: HTTP/MCP watch results differ:
  HTTP: 50 notifications, cursor dzE6MTMyMzguMTAwMA
  MCP: 100 notifications, cursor dzE6MTM1NDYuMTE5OQ
TestAllAssertionJudgmentsShareOneBudget
  topic_integration_test.go:595:
  Skipped:false Unchanged:false TopicsOpened:0 StancesWritten:4 Positions:4
  attempts=4 elapsed=1.630129584s
```

These findings were reported separately through the factory discovery record.
The configured integration gate has not passed in this audit. The service's
candidate check runs after hand-in; no service pass is claimed here. Publication
and a first live tag-triggered workflow run remain with the owner.
