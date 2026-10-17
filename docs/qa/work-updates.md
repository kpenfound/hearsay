# Work updates QA

Run the bounded script against the image built from the working tree:

```sh
dagger check
dagger api call hearsay qa --script "$(cat docs/qa/work-updates.sh)"
```

The script prints the candidate binary's SHA-256, migrates a disposable
Postgres database, and starts the four shipped services with a `work` source,
a poster agent and a human reader. It requires an unauthenticated post to
return 401, posts a project and workstream with the poster's token (202), reads
both actual distilled documents through `get_l1`, and resolves the workstream
entity through the API. Each wait has a deadline; failure prints service logs.

The model endpoint is a loopback HTTP fixture returning a descriptive summary
with outcome `none`. No external provider or source is contacted. Integration
tests separately replay the checked-in decision and assertion exchanges to
check standing. The poster is an observer, proving that posting does not
require a higher agent class.

In the factory runner, automatic workspace discovery finds no modules or
checks. The explicit equivalent uses the repository's existing `hearsay.qa`
function in the same live API session that uploads the workspace and serves
its module:

```sh
dagger --silent api with-session python3 docs/qa/dagger-workspace.py
dagger --progress plain api with-session python3 docs/qa/dagger-workspace.py --work-tests
dagger --progress plain api with-session python3 docs/qa/dagger-workspace.py --check
```

Unlike `--script @file`, the wrapper passes the script's contents. The
wrapper checks QA's final `exit code: 0` line, since an outer successful API
request alone does not prove that the script passed. `--work-tests` runs the
work API integration tests with race detection after fresh migrations;
`--check` evaluates the full configured Dagger check collection and fails if
the collection is empty or any result has an error. This is the explicit
workspace equivalent of `dagger check`, including generation checks.

Trial repeated on 2026-10-17 after the approved attribution and participant-role rulings:
Dagger v1.0.0-beta.17 built the working-tree candidate
with binary SHA-256
`06a060e34ad6a6d7d4c48ae264f746e0df17436cdba274fa4b380cd07014d27c`.
The explicit session completed successfully (outer exit 0), and the script
reported:

```text
PASS: work source, poster and four shipped services started
PASS: unauthenticated post refused with 401
PASS: authenticated project and workstream posts accepted with 202
PASS: API read-back of l1:projects:retry
PASS: API read-back of l1:projects:retry#policy
PASS: API resolves the posted workstream entity
exit code: 0
```

The recorded-fixture integration test checks the attributed update's human
event author, resolution to that principal and L1 author participation, plus
the poster's `agent` participation. Both attributed and unattributed updates
produce inferred standing under a policy that does not ratify their artifact
class automatically. A hand ratification through the shared gesture ledger
then makes the stance ratified. HTTP and MCP serve the same authors,
participants and standing. Normal distillation and startup recovery both
enqueue under the scope covering the root's touches. No authority code or
model recordings changed for the amendment.

The project and workstream are posted by `factory`; the attributed update is
posted by a separate configured attributor, `courier`, so its strongest L1 role
is `agent`. L1 keeps one role per principal: a poster that also opened the root
would retain the stronger `author` role. The fixture requests remain unchanged.

Final verification on 2026-10-17 passed: all 40 configured check results had
`error: null`, including lint and formatting, fresh schema-32 race integration,
Go tests (2,352 passed), tidy, both image platforms, Compose, Helm and generation.
The work API integration package passed in 26.876s in the final collection.
The standalone race-enabled work tests also passed, in 8.360s. The final
collection additionally checks the parsed current stance and standing tier.
Exact commands, statuses and relevant logs are retained in
[work-updates-checks.txt](work-updates-checks.txt).

Automatic `dagger check` reports `no checks selected` (exit 1) in this runner;
the explicit configured equivalent exits 0. The runner also supplies private
source file modes; normalizing file and directory readability lets the Compose
image's unprivileged user read the unchanged example configuration. No Dagger
module or production authority code was changed.

Earlier verification exposed an incorrect test expectation: when `factory`
posted both the root and an attributed update, L1 retained `factory` as
`author`, its strongest role, rather than `agent`. The test now uses the
separate `courier` attributor described above. No participant folding or
authority implementation changed. An earlier loaded race run also timed out
in Discord's existing `TestGatewayChannelUpdateRequestsResync` five-second
wait. The chief of staff retained that timing sensitivity as a suggestion;
no Discord code, waits or retries changed. The final configured run is the
verification result reported here.

How this was tried (for the pull request): Built the candidate through Dagger's
QA function with a disposable Postgres. Started a work source and all four
services, posted a project and workstream using an observer poster's token,
verified 401 without a token, read both distilled documents and resolved the
workstream over HTTP. Distillation used a local HTTP fixture with no external
model or source calls. The trial script and its outer command both exited 0.
