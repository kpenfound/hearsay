# Release workflow validation — 2026-11-04

This is validation before publication, not evidence of a GHCR release. No tag
was pushed, release created or image published, including to a local registry.
The first live GitHub Actions run, GHCR write and hosted-runner build belong to
the owner's first new tag push after merge.

The candidate adds `.github/workflows/publish.yml` and changes only the release
and workflow statements in CONTRIBUTING.md and AGENTS.md. It leaves the Dagger
module, lockfile, Go source, migrations and deployment behaviour unchanged.

## Static and shell validation

`actionlint` v1.7.12 passed `.github/workflows/publish.yml` with exit 0. Inspection
confirmed only `push.tags: ['v*']`, contents read/packages write permissions,
SHA-pinned checkout and Dagger install actions, an explicit CLI version, one
`hearsay publish` call, no other build or image tag, and no commit-gate checks.
The Dagger action v8.4.0's `action.yml` was inspected: with only `version` set,
it installs the CLI without executing its default `call` verb.

The exact workflow `run` body was extracted and executed under Bash with
throwaway `dagger` and `docker` stubs, a placeholder environment token and a
temporary summary file. These checked the publish arguments, environment
secret reference, digest-qualified smoke inputs and login via standard input.

| Case | Result |
| --- | --- |
| Both platforms report `hearsay v1.2.3 (commit unknown, built unknown)` | Exit 0 |
| Both platforms report `v1.2.30` for tag `v1.2.3` | Exit 1 |
| Only arm64 reports `v1.2.30` | Exit 1 |
| Missing arm64 platform | Nonzero |
| Wrong platform string | Exit 1 |
| Version execution failure | Nonzero |
| Publish failure | Nonzero; no smoke calls |

The digest appeared in the log and summary before each smoke failure. Only
the successful case appended the smoke-check success statement.

## Dagger execution

The CLI downloaded by the Dagger installer was explicitly
`v1.0.0-beta.25` (commit `a45241b2`). Execution used the matching
`ghcr.io/dagger/engine:v1.0.0-beta.25`, Docker 29.7.2, Linux aarch64. This
factory's file view has no workspace discovery boundary; a byte copy under
`/tmp/hearsay-publish-workspace` with an empty `.git` directory let native
workspace discovery load `dagger.toml`. No version-control command was used.
`dagger api functions hearsay` listed this repository's functions, including
`publish`, `image` and `image-check`.

The workflow's publish command was exercised with version `v1.2.3`, its actual
image address and `--password=env://HEARSAY_REGISTRY_TOKEN`, with a placeholder
token and an empty username. It resolved the secret, loaded and type-checked
`hearsay.publish`, then deliberately failed at `registry username is required`
before building or writing a registry. This CLI rejects the old `env:NAME`
secret syntax; `env://NAME` works. Other secret examples are assigned to the
contributor-docs audit.

Both exact `dagger -c` smoke command shapes were exercised on both platforms
with this public, digest-qualified fixture (not a Hearsay release):

```text
alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f
```

The platform calls returned `linux/amd64` and `linux/arm64`. The version calls
loaded and type-checked, then failed at execution because Alpine has no
`hearsay`. Replacing only the executable with `uname -m` succeeded on both
platforms (`x86_64`, `aarch64`), exercising native and emulated execution. A
request for amd64 from the arm64-only Alpine manifest failed with `no manifest
matches requested platform`. These pulls did not require credentials and do
not prove private GHCR authentication or publication.

The repository's actual image function was also executed, without publishing:

```sh
dagger api call hearsay image --version=v1.2.3 --platform=linux/arm64 \
  with-exec --args='hearsay,version' stdout
dagger api call hearsay image --version=v1.2.30 --platform=linux/arm64 \
  with-exec --args='hearsay,version' stdout
```

Both succeeded. Their outputs were `hearsay v1.2.3 (commit unknown, built
unknown)` and `hearsay v1.2.30 (commit unknown, built unknown)`. The workflow's
field comparison accepted the former and rejected the latter for `v1.2.3`.
The same image invocation for amd64 reached `go build` but failed twice with
Go runtime faults under this local arm64 runner's emulation. No production
build change was made to work around that environment limitation. Execution
on the selected `ubuntu-24.04` GitHub runner, including its arm64 emulation,
remains part of the first live run; neither that nor the full two-platform
Hearsay publish-and-pull sequence is claimed here.

GitHub's current Container registry and package visibility documentation was
checked: a newly published package defaults to private, public container
packages permit anonymous pulls, and the owner changes visibility through
Package settings → Danger Zone → Change visibility → Public. The release
docs record this one-time owner step.
