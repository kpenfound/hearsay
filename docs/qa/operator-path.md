# Operator path rehearsal — 2026-11-04

The README, operator guides, Compose env example and Helm value comments now
connect source setup to an actually published image, configuration and a running
deployment. This record separates local execution from the owner's first live
release. No tag was pushed, release created or image published.

## Release and documentation audit

The merged `.github/workflows/publish.yml` triggers only on pushed `v*` tags,
calls the existing Dagger `hearsay publish` with the tag as its version, and
targets `ghcr.io/kpenfound/hearsay:<tag>`. The unchanged module builds one
`linux/amd64` and `linux/arm64` manifest; the workflow pushes no `latest` or
additional tag. Its log and Summary show the digest-qualified address before
both-platform smoke checks. Use a successful run and the GHCR package versions,
not a repository tag alone, to select an image. Old tags are not retroactively
published. Concrete versions are explicitly examples.

These facts were cross-checked across CONTRIBUTING.md, AGENTS.md, both operator
guides and deployment comments, against the workflow and the Dagger module.
The manual publish path for forks remains documented. The owner must set the
package visibility to Public after the first publish for anonymous pulls.

The README now names all eight registered sources, including `work`, describes
Drive change sync and Obsidian polling as shipped, and links both deployment
paths. Its status and next work agree with the design reference. Read-path prose
accounts for optional query embedding; the shipped provider catalogue currently
has no embedding adapter.

Operator commands and environment descriptions were traced to
`cmd/hearsay/init.go`, `internal/onboard`, `runConfig` and the source
registry in `cmd/hearsay/run.go`, connector implementations, Compose, Helm and
`.dagger/modules/hearsay/main.dang`. Corrections found during the walk include:

- The checkout already has `config/hearsay.yaml`, so plain `init` refuses it.
  First-setup examples now explain `--force`, including preserving prior tokens
  or configuration before replacement.
- Config files/directories must be readable/traversable by `nobody`; the env
  file stays private. Permissions are set before container validation.
- Optional Slack identity lookup needs its bot credential forwarded into the
  initialization container. An agent principal must be added before using the
  example agent authentication headers; `init` creates humans.
- Exported `HEARSAY_IMAGE` overrides `.env` during Compose interpolation.
  The upgrade procedure clears an earlier export so the file selects the image.
- The webhook table includes generic tracker senders, and the token descriptions
  account for work-source posters. `docker compose ps -a` includes the exited
  migration job when checking the deployment.

## Working-tree image execution

Used Dagger CLI v1.0.0-beta.25 on Linux arm64. The module's Docker CLI image
reported Compose v2.40.3, matching the guide's v2.40 reference. The factory view needed a
byte-copied temporary workspace with an empty `.git` directory as a discovery
boundary; no version-control operation was used. Runtime source and deployment
configuration were unchanged. Dagger's `qa` and `compose-smoke` built the native
image from that working tree; no GHCR image was assumed or pulled.

`dagger api call hearsay qa --script '<script contents>'` ran the following
plan in the shipped image. The plan contents were passed as a single argument,
not `@file`. Postgres was attached by `qa`, but its database URL was unset
before initialization/validation. All source credentials were absent.

```sh
set -eu
hearsay version
unset HEARSAY_DATABASE_URL HEARSAY_GITHUB_TOKEN HEARSAY_DISCORD_TOKEN \
  HEARSAY_DRIVE_CREDENTIALS HEARSAY_SLACK_BOT_TOKEN
mkdir -p /tmp/operator-final/config
cd /tmp/operator-final
printf 'sources: []\n' > config/hearsay.yaml
init_team() {
  hearsay init --no-input "$@" \
    --operator kyle --operator-name "Kyle Penfound" --operator-github kpenfound \
    --github-repo acme/api \
    --discord-guild 824100000000000000 --discord-channel 824100000000000001 \
    --drive-folder 1AbCdEfGhIjKlMnOpQrStUvWxYz \
    --out config/hearsay.yaml --env-file hearsay.env
}
init_team --force
chmod -R a+rX config
hearsay config validate config
su nobody -s /bin/sh -c 'hearsay config validate /tmp/operator-final/config'
test "$(stat -c %a config/hearsay.yaml)" = 644
test "$(stat -c %a hearsay.env)" = 600
before=$(sha256sum config/hearsay.yaml hearsay.env)
if init_team > /tmp/refusal.txt 2>&1; then exit 1; fi
grep 'already exists' /tmp/refusal.txt
grep 'nothing was written' /tmp/refusal.txt
test "$before" = "$(sha256sum config/hearsay.yaml hearsay.env)"
printf 'operator rehearsal: ok\n'
```

Observed `hearsay devel (commit unknown, built unknown)`, as expected for an
unstamped file-only build. Initialization produced GitHub, Discord and Drive
sources, scope `api`, principal `kyle`, and an env file naming its generated
`HEARSAY_KYLE_TOKEN`. It listed the empty source/model credentials to fill and
reported that collaborators were not read without the GitHub token.
Validation succeeded both as root and as `nobody`, without credentials or a
database, reporting three sources, one scope, one principal, one code entity,
two model tiers and digest
`sha256:74f500f03477fc7eebb1cc182b86dc4c1524aa8cae472c16f5b26f140170a8ac`.
A repeated init without force refused both existing files and changed neither.
The final transcript ended with `operator rehearsal: ok` and `exit code: 0`.
The inner status was checked; the outer command alone is not proof of success.

## Compose and Helm verification

The first `dagger api call hearsay compose-smoke` reached the services but
failed with configuration permission errors: the factory copy had a mode-0700
config directory and mode-0600 example YAML. Applied the documented
`chmod -R a+rX config` only to the temporary copy's `deploy/compose/config`.
No repository mode or content was changed to bypass the failure.

The rerun passed using the native working-tree image and an isolated Docker
daemon. Its output included:

```text
api             running healthy 0
assert-worker   running healthy 0
connectors      running healthy 0
distiller       running healthy 0
migrate         exited          0
postgres        running healthy 0
published: api 127.0.0.1:8080->8080, connectors 127.0.0.1:8081->8081
get_bundle as operator: 200
compose smoke: ok
```

The smoke script generates distinct tokens, rewrites `HEARSAY_IMAGE` to the
locally built image and supplies a dummy model key. Its agent-session-only
configuration makes no source or provider call. This proves migrations,
readiness, published-port restrictions, mounted config, credentials reaching
the API and the authenticated bundle request; it does not prove useful
source-derived content.

`dagger check hearsay:compose-check hearsay:helm-check` passed both checks in
the normalized copy. These use Compose's parser and Helm lint/template,
including install/upgrade shapes, config validation, digest rendering and
required-value refusals. No Kubernetes cluster was used.

## Link and change boundaries

A one-off Markdown parser walk checked every rendered relative link and fragment
in the 31 scoped Markdown files: README.md, CONTRIBUTING.md, AGENTS.md, the six
named design/config/connector/operator/evaluation documents, the ADR index,
the command README and all 20 package/fixture READMEs under `internal/`.
All 233 relative links and anchors resolve, including links to directories and
fragments in other Markdown files. Fenced examples were excluded; GitHub heading
slugs, duplicate suffixes and explicit HTML anchors were accounted for. This
needed no permanent repository check or dependency.

SHA-256 comparisons against the pre-edit workspace show only README.md, the
two operator guides, `deploy/compose/env.example` and Helm `values.yaml` changed,
plus this new evidence record. Reversing only the comment replacements in the
two deployment files reproduces their original hashes: every env assignment
and Helm value is identical. Go source, migrations, Compose manifests, Helm
templates, chart metadata, the workflow, lockfile, accepted ADR bodies and prior
QA records remain byte-identical. The example image assignment retains the
shape required by `compose-check`; `compose-smoke` replaces it locally.

## Not exercised

No live GitHub, Discord, Slack, Drive, Obsidian, tracker, session or work sender
was exercised. No model provider, source backfill, meaningful distillation,
external TLS/ingress or Kubernetes rollout was tested. No tag-triggered publish,
GHCR push/pull, package-visibility change or hosted workflow run occurred.
Those release operations remain with the owner. This unit ran the two relevant
deployment checks, not a fresh full commit gate; the earlier contributor audit's
integration failures remain recorded in its separate evidence.
