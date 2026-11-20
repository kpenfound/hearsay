#!/bin/sh
# Runs the owner-decision call (`decide`) end to end in containers, with Docker
# alone.
#
# It starts the pgvector Postgres Hearsay's tests use, then runs
# TestDecideEndToEnd (cmd/hearsay/decide_e2e_integration_test.go) from this tree
# in the Go image go.mod asks for, on a network with that Postgres. The test runs
# `hearsay all` — connectors, distiller, assertion worker and API — posts a work
# decision to /hooks/<source>, and then, as the owner and as an agent (`kern`,
# class worker) carrying the owner's decision:
#
#   - ratifies the decision's stance over HTTP, demotes it over MCP with the
#     agent carrying it, replays it over both interfaces, and undoes both;
#   - after each step reads the topic's tier from stance_history, get_bundle and
#     traverse (ratified, contested, ratified, inferred);
#   - has an observer refused with not_allowed, and the other refusal codes and
#     a bad credential (no code) answered as documented;
#   - reads the ledger with `hearsay gestures list`, in its table and --json,
#     where each decision shows its principal and its carrier.
#
# Its output is the evidence, and it is container evidence because every process
# ran in those two containers. The model provider is a local stub (the test says
# so) and the test fails if the decide path calls it.
#
#   deploy/smoke/decide-e2e.sh [evidence-dir]
#
# Run it from the repository root. The raw get_bundle and traverse outputs and
# the gestures list are written to evidence-dir (default: a new temporary
# directory, printed at the end). Without Docker, run the test against a
# Postgres of your own:
#
#   HEARSAY_DATABASE_URL=postgres://... go test -tags=integration -count=1 -v \
#       -run TestDecideEndToEnd ./cmd/hearsay
#
# `dagger check` runs the same test with the rest.
set -eu

root=$(pwd)
[ -f "$root/go.mod" ] && [ -d "$root/cmd/hearsay" ] || { echo "run this from the repository root" >&2; exit 2; }
goversion=$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' go.mod)
evidence=${1:-$(mktemp -d)}
mkdir -p "$evidence"
evidence=$(cd "$evidence" && pwd)
suffix=$$
network=hearsay-decide-e2e-$suffix
postgres=hearsay-decide-e2e-pg-$suffix

cleanup() {
	docker rm -f "$postgres" >/dev/null 2>&1 || true
	docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker network create "$network" >/dev/null
docker run -d --name "$postgres" --network "$network" \
	-e POSTGRES_USER=hearsay -e POSTGRES_PASSWORD=hearsay -e POSTGRES_DB=hearsay \
	pgvector/pgvector:pg16 >/dev/null
tries=0
until docker exec "$postgres" pg_isready -U hearsay >/dev/null 2>&1; do
	tries=$((tries + 1))
	[ "$tries" -lt 60 ] || { echo "postgres did not come up" >&2; exit 1; }
	sleep 1
done

echo "== container evidence: Postgres (pgvector/pgvector:pg16) and Go $goversion in Docker, network $network"
docker run --rm --network "$network" \
	-v "$root":/src -w /src \
	-v hearsay-e2e-gomod:/go/pkg/mod -v hearsay-e2e-gobuild:/root/.cache/go-build \
	-v "$evidence":/evidence -e HEARSAY_E2E_EVIDENCE=/evidence \
	-e HEARSAY_DATABASE_URL="postgres://hearsay:hearsay@$postgres:5432/hearsay?sslmode=disable" \
	-e HTTP_PROXY -e HTTPS_PROXY -e NO_PROXY="${NO_PROXY:-},$postgres" \
	"golang:$goversion" \
	go test -tags=integration -count=1 -v -run TestDecideEndToEnd ./cmd/hearsay
echo "== raw get_bundle, traverse and gestures outputs: $evidence"
