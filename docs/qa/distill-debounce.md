# Distill debounce QA

Run the bounded script against the image built from the working tree, with a
throwaway Postgres beside it:

```sh
dagger check
dagger api call hearsay qa --script "$(cat docs/qa/distill-debounce.sh)"
```

The script migrates the disposable database, checks that both `distiller` and
`all` refuse `HEARSAY_DISTILL_QUIET=2s` with
`HEARSAY_DISTILL_MAX_DELAY=1s`, then starts the distiller with valid settings
(`5s` quiet, `10s` maximum). It inserts ten L0 revisions of one issue, waits
for the shipped pump to consume them and stops the service before the job is
claimed. SQL assertions require all ten events to be consumed, exactly one
pending distill job, no attempts, and `run_after - enqueued_at = 5s`.
The model key is a dummy and both model endpoints point at an unreachable
loopback port. This exercise makes no model or source call. The integration
suite separately verifies execution and one model call through recorded fakes.

Verified on 2026-10-15 with Dagger v1.0.0-beta.17, Go 1.27.1 and pgvector
Postgres 16. The QA transcript reports:

```text
PASS: distiller refused maximum below quiet
PASS: all refused maximum below quiet
PASS: distiller started with valid settings
INSERT 0 10
DO
  kind   |          target_id          |  state  | attempt |  quiet
---------+-----------------------------+---------+---------+----------
 distill | l1:debounce-qa:acme/api#235 | pending |       0 | 00:00:05
(1 row)

PASS: ten revisions consumed, one pending distill job, configured quiet 5s
exit code: 0
```

The last line is QA's **script status**, not the outer Dagger exit.

In the factory runner, automatic workspace discovery returned no configuration
or checks, even with an explicit local workspace path. Verification therefore
used an explicit `host.directory(path: ..., gitignore: false).asWorkspace`
upload with `withConfigPaths(configFile: "dagger.toml", lockFile:
"dagger.lock")`. The full configured check collection was evaluated with
`artifacts.filterTypes(types: ["Check"]).values(maxConcurrency: 4)`: all 38
results had `error: null`, including Compose, Helm, image, integration, lint,
tidy and generation checks; the Go test summary reported 1,834 passed. QA was
called through the existing `hearsay.qa(ws: ..., script: ...)` function in the
same live API session that uploaded the workspace and served its module.
No Dagger module was changed.

The runner's original source files have private `0600`/`0700` permissions,
which prevented the Compose image's `nobody` user from reading its example
configuration. A disposable copy with identical file contents and readable
`0644`/`0755` permissions resolved that runner-only failure. Uploaded contents
were compared with the candidate before accepting the results.
