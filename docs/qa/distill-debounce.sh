#!/bin/sh
# Run with: dagger api call hearsay qa --script "$(cat docs/qa/distill-debounce.sh)"
# Disposable qa database only. No live model or source is contacted.
set -eu
pid=
cleanup() {
  if [ -n "$pid" ]; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" || true
  fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
hearsay version
hearsay migrate up

# Both entry points reject the environment settings before starting services.
for command in distiller all; do
  if timeout 5 env HEARSAY_DISTILL_QUIET=2s HEARSAY_DISTILL_MAX_DELAY=1s \
      hearsay "$command" > /tmp/refusal.log 2>&1; then
    echo "FAIL: $command accepted maximum below quiet"; exit 1
  fi
  grep -F 'must be at least --distill-quiet' /tmp/refusal.log
  echo "PASS: $command refused maximum below quiet"
done

# Build real model tiers with a dummy key and an unreachable local endpoint.
# Stop while the job is pending, before any model invocation is possible.
cat > /tmp/debounce.yaml <<'YAML'
sources:
  - id: debounce-qa
    type: github
    containers: [acme/api]
scopes:
  - id: qa
    name: Debounce QA
    sources: [debounce-qa]
llm:
  tiers:
    distill:
      base_url: http://127.0.0.1:1
    assert:
      base_url: http://127.0.0.1:1
YAML
export ANTHROPIC_API_KEY=qa-dummy
export HEARSAY_DISTILL_QUIET=5s
export HEARSAY_DISTILL_MAX_DELAY=10s
hearsay distiller --config /tmp/debounce.yaml > /tmp/distiller.log 2>&1 &
pid=$!
i=0
until grep -q 'distiller started' /tmp/distiller.log; do
  kill -0 "$pid" || { cat /tmp/distiller.log; exit 1; }
  i=$((i + 1)); [ "$i" -le 5 ] || { cat /tmp/distiller.log; exit 1; }
  sleep 1
done
echo 'PASS: distiller started with valid settings'

# Seed ten contract-shaped revisions of one issue in one committed burst.
# The shipped pump consumes this feed and writes the real queue transaction.
psql -v ON_ERROR_STOP=1 <<'SQL'
INSERT INTO l0_events
  (id, source, native_id, kind, artifact, revision_token, revision_edited_at,
   occurred_at, payload, acl)
SELECT 'evt:debounce-qa:acme/api#235@r' || n, 'debounce-qa',
       'acme/api#235@r' || n, 'issue', 'acme/api#235', 'r' || n,
       '2026-09-09T12:00:00Z'::timestamptz + n * interval '1 second',
       '2026-09-09T12:00:00Z'::timestamptz,
       jsonb_build_object('artifact', 'acme/api#235',
         'revision', jsonb_build_object('token', 'r' || n, 'edited_at',
           to_char('2026-09-09T12:00:00Z'::timestamptz + n * interval '1 second',
                   'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
         'container', jsonb_build_object('kind', 'repository', 'native_id', 'acme/api'),
         'url', 'https://github.com/acme/api/issues/235',
         'title', 'Debounce QA', 'text', 'Revision ' || n),
       '[{"kind":"public"}]'::jsonb
FROM generate_series(1, 10) AS n;
SQL

i=0
while [ "$(psql -At -v ON_ERROR_STOP=1 -c "SELECT count(*) FROM queue_job WHERE kind='distill'")" = 0 ]; do
  kill -0 "$pid" || { cat /tmp/distiller.log; exit 1; }
  i=$((i + 1)); [ "$i" -le 4 ] || { cat /tmp/distiller.log; exit 1; }
  sleep 1
done
# Freeze the observation before quiet expires; graceful shutdown runs no job.
kill -TERM "$pid"
wait "$pid"
pid=

psql -v ON_ERROR_STOP=1 <<'SQL'
DO $$
BEGIN
  IF (SELECT count(*) FROM l0_events WHERE source='debounce-qa') <> 10 THEN
    RAISE EXCEPTION 'expected ten input events';
  END IF;
  IF (SELECT count(*) FROM queue_job WHERE kind='distill') <> 1 THEN
    RAISE EXCEPTION 'expected exactly one distill job';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM queue_job
      WHERE kind='distill' AND target_id='l1:debounce-qa:acme/api#235'
        AND state='pending' AND attempt=0
        AND run_after-enqueued_at=interval '5 seconds') THEN
    RAISE EXCEPTION 'expected one unclaimed job due after the configured quiet period';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM l0_feed_cursors
      WHERE consumer='distiller' AND seq=(SELECT max(seq) FROM l0_events)) THEN
    RAISE EXCEPTION 'pump did not consume the complete burst';
  END IF;
END $$;
SELECT kind, target_id, state, attempt, run_after-enqueued_at AS quiet
FROM queue_job;
SQL
echo 'PASS: ten revisions consumed, one pending distill job, configured quiet 5s'
