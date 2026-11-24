#!/bin/sh
# Runs Kern's conversation as memory end to end in containers, with Docker alone.
#
# It starts the pgvector Postgres Hearsay's tests use, then runs
# TestConversationMemoryEndToEnd (cmd/hearsay/conversation_e2e_integration_test.go)
# from this tree in the Go image go.mod asks for, on a network with that
# Postgres. The test runs `hearsay all` (connectors, distiller, assertion worker
# and API) on the conversation source docs/config.md documents, plus a second
# human, an agent acting for them, and a public ticket in the same scope and
# topic, and then:
#
#   - posts a multi-topic conversation to /hooks/<source id> as the agent that
#     is allowed to post, one message per request, the owner's decision on the
#     ticket's topic among them;
#   - polls `coverage` from pending to covered, and finds the conversation with
#     `search` (since/until) by a word only its distilled summary holds;
#   - reads the owner's stance through get_bundle, traverse and stance_history,
#     and the second human's and their agent's reads (search, get_l0, get_l1,
#     get_bundle, traverse, stance_history, coverage, watch), which hold the
#     ticket and none of the conversation;
#   - deletes the decision's exchange, sees it excluded, and, after the
#     distiller's rebuild, its words in none of search, get_l0, get_l1,
#     get_bundle or any stance's position;
#   - posts it again, as it was and rewritten, and sees nothing written.
#
# Its output is the evidence, and it is container evidence because every process
# ran in those two containers. The model provider and Jev are a local stub (the
# test says so); no real model, Jev or live source is reachable from the test.
#
#   deploy/smoke/conversation-memory-e2e.sh [evidence-dir]
#
# Run it from the repository root. The raw reads are written to evidence-dir
# (default: a new temporary directory, printed at the end). Without Docker, run
# the test against a Postgres of your own:
#
#   HEARSAY_DATABASE_URL=postgres://... go test -tags=integration -count=1 -v \
#       -run TestConversationMemoryEndToEnd ./cmd/hearsay
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
network=hearsay-conversation-e2e-$suffix
postgres=hearsay-conversation-e2e-pg-$suffix

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
	go test -tags=integration -count=1 -v -run TestConversationMemoryEndToEnd ./cmd/hearsay
echo "== raw coverage, search, stance_history, get_bundle, traverse and delete outputs: $evidence"
