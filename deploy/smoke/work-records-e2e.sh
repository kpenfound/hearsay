#!/bin/sh
# Runs the owner/work decision path end to end in containers, with Docker alone.
#
# It starts the pgvector Postgres Hearsay's tests use (the same image the Dagger
# module binds), then runs TestWorkRecordsEndToEnd
# (cmd/hearsay/work_records_e2e_integration_test.go) from this tree in the Go
# image go.mod asks for, on a network with that Postgres. The test runs
# `hearsay all` — connectors, distiller, assertion worker and API — posts the
# owner/work records through /hooks/<source> and reads them back as the owner
# through get_bundle and traverse; its output is the evidence, and it is
# container evidence because every process ran in those two containers. The
# model provider is a local stub (the test says so).
#
#   deploy/smoke/work-records-e2e.sh [evidence-dir]
#
# Run it from the repository root. The raw get_bundle and traverse outputs are
# written to evidence-dir (default: a new temporary directory, printed at the
# end). Without Docker, run the test against a Postgres of your own:
#
#   HEARSAY_DATABASE_URL=postgres://... go test -tags=integration -count=1 -v \
#       -run TestWorkRecordsEndToEnd ./cmd/hearsay
#
# `dagger check hearsay:integration-test` runs the same test with the rest.
set -eu

root=$(pwd)
[ -f "$root/go.mod" ] && [ -d "$root/cmd/hearsay" ] || { echo "run this from the repository root" >&2; exit 2; }
goversion=$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' go.mod)
evidence=${1:-$(mktemp -d)}
mkdir -p "$evidence"
evidence=$(cd "$evidence" && pwd)
suffix=$$
network=hearsay-e2e-$suffix
postgres=hearsay-e2e-pg-$suffix

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
	go test -tags=integration -count=1 -v -run TestWorkRecordsEndToEnd ./cmd/hearsay
echo "== raw get_bundle and traverse outputs: $evidence"
