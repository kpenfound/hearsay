---
name: behavioral-testing
description: Design, write, review and run tests that protect application behavior through implementation changes. Use when choosing test boundaries, adding regression coverage, designing E2E scenarios, maintaining golden files or improving test reliability and execution time.
---

# Behavioral testing

Protect meaningful application contracts through substantial implementation
changes. Before adding a test, identify the guarantee it protects and the
incorrect behavior that would make it fail. Apply this guidance to the work at
hand; audit or restructure the broader suite only when requested.

## Choose the boundary

- Default to E2E tests for application behavior. Exercise the real built
  application through its public interface, usually as a separate process or
  service. Observe meaningful output, persisted state or externally visible
  effects. A success status alone rarely establishes the whole contract.
- Unit tests must justify their existence. Use them for durable contracts where
  they add meaningful coverage that is difficult or expensive to obtain through
  E2E, such as extensive parser cases or algorithmic invariants. Being easy to
  write, increasing a coverage percentage or asserting a helper's current shape
  is insufficient justification.
- Focused component or controller tests can cover decisions and failure variants
  economically. Retain E2E evidence for the application wiring and integration
  contracts those tests bypass. Do not call a helper snapshot E2E merely because
  it uses a golden file.
- Ask whether a substantial implementation replacement could preserve the test.
  Avoid assertions on private calls, internal layout or duplicated implementation
  logic unless that detail is itself a maintained contract.
- Test current requirements. Do not add permanent tests proving that an obsolete
  name, source fragment or behavior was removed merely to verify a patch. Use a
  one-time inspection for mechanical cleanup. Add a permanent check only when
  the absence itself is an explicit maintained requirement, such as a packaging
  rule, an architectural boundary, or the repository's lint and markdown checks.
  Negative assertions are valuable when they protect a current contract, such as
  denied access, secret omission or prevention of duplicate side effects. The
  `no-deltas` skill, when installed, states the same rule for tests in general.
- Preserve useful existing coverage. These principles do not authorize wholesale
  deletion of unit tests or migration of the project's test infrastructure.

## Design independent scenarios

One scenario should establish one recognizable contract, not necessarily perform
one action or contain one assertion. Creating a record, restarting the application
and reading it back can jointly prove persistence. Adding search, export and
deletion checks to that scenario usually combines unrelated contracts.

- Give each scenario its own prerequisites so it can run alone and in any order.
  Name it after the behavior it protects. Do not consume another test's output.
- Start near the behavior under test. Choose the cheapest reliable setup path
  that produces valid state without bypassing the behavior being verified.
- Use existing fixture builders or the project's ORM when accessible and useful.
  Do not expose internals or restructure production code just to import setup
  helpers. Database seeds are a valid fallback. A running API is appropriate
  when needed for valid setup or when already convenient and inexpensive; it is
  not mandatory for prerequisites.
- Include all required fixture state. A search fixture may need both database
  rows and index entries. A creation scenario must exercise creation through the
  application interface; seeding the resulting record would bypass its claim.
- Share expensive infrastructure where appropriate, but isolate mutable state
  with separate databases, namespaces, directories or equivalent boundaries.
  The layer that starts a resource owns cleanup, including failure paths.
- Use real dependencies for the integration behavior being tested. Controlled
  substitutes can bound costly, nondeterministic or externally mutating systems;
  be explicit about the boundary they leave unverified. Do not replace the very
  interaction the scenario claims to establish with an unconditional fake success.
- Add a long journey only when the sequence itself establishes a distinct
  integration contract. Before adding variants, identify what existing journeys
  and focused tests cannot establish.

## Make assertions independent

Expected outcomes should come from requirements, independently established
examples or reviewed fixtures. Do not derive expectations by repeating the
implementation or calling the same transformation on both sides of an assertion.
E2E tests can also validate themselves if their expectations merely accept
whatever the application produces.

Prefer focused assertions when they express the contract clearly. Use golden
files when the complete shape of rich output matters: TUI screens, CLI tables,
diagnostics, generated files, serialized documents or exports. Keep fixtures
small enough for meaningful review. When appropriate, also parse, compile or
otherwise validate generated artifacts; matching a fixture alone does not
establish their validity.

### Golden files and nondeterminism

- Choose a representation that protects the intended contract. Parsed JSON can
  ignore irrelevant whitespace; a CLI layout may require exact spacing. TUI
  text, styling and terminal screen state protect different properties. Rendering
  coverage does not establish keyboard or interaction behavior.
- Control variation at its source when practical: clock, random seed, locale,
  terminal dimensions and similar inputs.
- Otherwise normalize narrowly, such as replacing a timestamp with `<TIMESTAMP>`.
  Preserve surrounding content and meaningful relationships. Do not mask time
  values in a timestamp-formatting test, sort arrays whose order matters or drop
  whole lines merely because one field varies.

### Golden update controls

- Verification must not rewrite expectations. Use a separate explicit update
  command or mode; CI verifies without updating.
- Investigate mismatches before regenerating. Distinguish regressions, intended
  behavior changes and uncontrolled variation. A mismatch does not establish
  that the expected output is wrong.
- An agent may update goldens when the changes follow from the user's intended
  behavior. No separate approval is required for those updates. A request to
  make tests pass does not by itself justify accepting changed output.
- Update affected fixtures and inspect the actual diff against the intended
  contract. Explain or fix unexpected differences. Review initial goldens too:
  generated output records what the application does, not proof of correctness.
- Run verification with updates disabled after accepting changes. Explain
  meaningful expectation changes in the PR or completion report, and flag
  uncertainty. Human review is an additional check, not a substitute for the
  agent reviewing the diff.

## Keep execution reliable and efficient

Read the repository's testing instructions and use its canonical environment.
When using Dagger, read [Dagger orchestration](references/dagger.md) for lifecycle
patterns and focused execution guidance. Dagger is an option, not a prerequisite.

- Build once and reuse artifacts and expensive setup where valid. Keep the
  application and test inputs tied to the change being verified.
- During iteration, select the relevant scenarios while preserving the full
  runtime environment, dependency versions and execution boundaries. Tests should
  be discoverable and individually runnable. Do not bypass required orchestration
  merely to run one test faster.
- Before finishing, expand validation to the affected contracts and run the
  repository's required completion checks. A focused pass does not replace a
  mandated full suite. After checks pass, repeat them only when new changes,
  failures or unresolved concerns warrant it.
- Use readiness checks and bounded condition waits rather than arbitrary sleeps.
  Coordinate controlled events explicitly. Assert elapsed time when timing is
  the contract, not as an indirect guess that background work has finished.
- Fail with the unmet expectation and relevant application output, service logs
  or state. Preserve enough evidence to diagnose a failure before cleanup.
- Investigate flaky tests. Do not retry until green or hide intermittent failures.
  Repeated execution and race detection are diagnostic modes when relevant.
- Parallelize isolated scenarios within available resources. Measure contention;
  more parallelism can make the suite slower or less reliable.
- Profile before optimizing. Compare runs with consistent resources, settings
  and cache conditions. Separate image pulls, engine startup and compilation from
  scenario execution. Distinguish fresh execution from cached results, especially
  for performance or flake investigations. In-memory fixtures can speed ordinary
  state tests but cannot prove physical crash durability.

## Review the evidence

For each changed test, ask: **What meaningful failure would this catch, and what
does its execution boundary leave unverified?** Check that setup, normalization,
fakes and expected values do not bypass that claim.

Report the checks actually run and their results, including relevant skips,
cached results and blockers. Do not claim broader coverage than the executed
scenarios provide.
