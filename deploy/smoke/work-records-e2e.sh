#!/bin/sh
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
