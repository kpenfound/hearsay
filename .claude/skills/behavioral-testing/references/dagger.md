# Dagger orchestration for behavioral tests

Use the repository's existing Dagger entrypoints, generated clients and pinned
dependencies. Inspect its documentation and module before choosing commands;
selection flags and functions are project-specific. Do not introduce Dagger or
migrate an established setup merely because this reference describes it.

## Choose lifecycle ownership

Both patterns can exercise the real built application as a separate process.
Choose based on the project's conventions and the control scenarios need.

| Pattern              | Dagger module owns                                                                                                       | Test code owns                                                                                                           |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------ |
| Compose-style        | Building and starting dependent services, preparing the test runtime and running tests in it                             | Scenario prerequisites, application interactions and assertions                                                          |
| Testcontainers-style | Building the test environment and exposing orchestration capabilities, without starting the scenario dependencies itself | Managing service dependencies through generated Dagger clients, plus scenario prerequisites, interactions and assertions |

Compose-style is convenient for a stable shared service topology.
Testcontainers-style gives scenarios direct control over dependency configuration
and lifecycle, such as restarting a service to verify recovery. Neither requires
a fresh dependency instance per scenario: decide service lifetime separately
from mutable-state isolation. Keep ownership and cleanup explicit rather than
splitting responsibility ambiguously between the module and tests.

Use orchestration helpers to arrange the environment. Assertions should still
describe application contracts. Stopping a dependency is meaningful when it
establishes recovery, bounded failure or preserved data, not merely when it
exercises orchestration code.

## Select scenarios without changing the environment

A useful project interface supports listing checks, selecting individual tests
or groups, and rerunning a reported failure in the same runtime as the full
suite. Preserve installed tools, service configuration and application artifacts
when narrowing selection.

For example, Osmia documents these commands through its configured Go check:

```sh
dagger check --progress=report --go-test=TestA
dagger check --progress=report --go-test=TestA --go-test=TestB
dagger check --progress=report --go-package=internal/service
dagger check --progress=report --go-package=internal/service --go-test=TestA
```

Its focused runs retain the full test container's pinned tools and browser. It
also supports rerunning a quoted check link from a failure report, diagnostic
race and repeated-run environments, and requires the full
`dagger check --progress=report` before completion. These are examples of a
repository contract, not flags or requirements to assume in every Dagger project.
The skill does not require an Osmia checkout.

Follow the current project's completion requirements. For diagnosis, use concise
check results first and detailed execution output when needed to identify the
actual test command or environment problem.

## Measure the right work

Reuse builds and dependency setup where inputs permit. Ensure changed source,
fixtures and configuration are included in the inputs being tested. Understand
both orchestration caching and the language runner's test-result caching; a
fresh flake or performance investigation must actually execute the scenarios.

Measure inside the canonical runtime with consistent CPU allocation, concurrency,
instrumentation and cache conditions. Compare complete-suite or group durations
as well as individual scenarios. Account separately for image pulls, engine
startup and compilation. Local timings characterize that environment, not the
speed or contention of a different CI engine.
