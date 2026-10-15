# Artifact state binary trial

From the repository root, with Go, Python 3, Dagger and a container runtime:

```sh
python3 internal/l1/testdata/state-qa/run.py
```

The runner uploads the working tree and invokes the existing `hearsay.qa`
function through a live Dagger API session. This is the same shipped-image,
Postgres-backed sandbox as `dagger api call hearsay qa --script ...`. It starts
`hearsay all`, posts a generic tracker ticket, waits for distillation, reads the
state through authenticated `get_l1` and `search`, and checks the exported
`ListFor` filter with a static helper built from the working tree. No production
endpoint, connector, prompt, bundle or image is changed for this trial. The
local HTTP model fixture supplies a recorded body and refuses a request that
exposes the source's state label. No live model or tracker is contacted.

The runner checks the QA transcript's exit code, because `qa` returns a
script's failure as text. `DAGGER_ENGINE` selects an engine when needed; the
helper targets Linux and respects `GOARCH` if the engine architecture differs
from the local Go toolchain.

This change was tried with:

```sh
DAGGER_ENGINE=image://ghcr.io/dagger/engine:v1.0.0-beta.17 \
  python3 internal/l1/testdata/state-qa/run.py
```

The successful transcript includes:

```text
get_l1: l1:qa:ENG#QA-1 {'core': 'started', 'native': 'In review'}
search: l1:qa:ENG#QA-1 {'core': 'started', 'native': 'In review'}
ListFor(started): 1 documents
ListFor(completed): 0 documents
exit code: 0
```

Combined verification was attempted with `dagger check`. This unversioned
factory view's implicit workspace selected no checks, so all 38 were also run
with an explicit uploaded workspace:

```sh
dagger --engine image://ghcr.io/dagger/engine:v1.0.0-beta.17 -c \
  'host | directory . | as-workspace | artifacts | filter-types --types=Check | values --fail-fast=false'
```

34 passed initially. SDK generation could not resolve `dagger.io`; the image
and integration checks exhausted the 10 GB Docker backing disk; compose's
configuration fixture inherited the factory view's private file/directory
permissions. After restoring read/traverse permission on that fixture,
`compose-check --ws .` passed. The outstanding gate is a clean full
`dagger check` in an environment with working DNS and sufficient disk space.

The documented direct fallback passed: `go build ./...`, `go test ./...`,
`go test -race ./...`, `go vet ./...`, `go run ./cmd/hearsay help`, and
`golangci-lint run` plus `golangci-lint fmt` using the repository's v2.13.2
linter image. Race-enabled database, L1, distiller and API integration suites
passed against a fresh Postgres 16/pgvector database migrated with the changed
binary:

```sh
# HEARSAY_DATABASE_URL names a fresh database with CREATE DATABASE permission.
go run ./cmd/hearsay migrate up
CGO_ENABLED=1 go test -race -tags=integration \
  ./internal/db ./internal/l1 ./internal/service/distiller ./internal/service/api
```

The final combined fallback also passed `CGO_ENABLED=1 go test -race
-tags=integration ./...` on another fresh migrated database, including both
neighboring connector units and every service and layer.
