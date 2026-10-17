#!/bin/sh
# dagger api call hearsay qa --script "$(cat docs/qa/work-updates.sh)"
# Runs the built candidate with a disposable database. The only model endpoint
# is a local HTTP fixture; no provider or live source is contacted.
set -eu
pids=
cleanup() {
  for pid in $pids; do kill -TERM "$pid" 2>/dev/null || true; done
  for pid in $pids; do wait "$pid" || true; done
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
apk add --no-cache curl jq python3
hearsay version
sha256sum /usr/local/bin/hearsay
hearsay migrate up
cat > /tmp/work.yaml <<'YAML'
sources:
  - id: projects
    type: work
    containers: ['*']
    settings:
      posters: [factory]
      attributors: [factory]
principals:
  - id: factory
    kind: agent
    class: observer
    token_env: QA_POSTER_TOKEN
    scopes: ['*']
  - id: reader
    kind: human
    token_env: QA_READER_TOKEN
    scopes: ['*']
scopes:
  - id: work
    sources: [projects]
llm:
  tiers:
    distill:
      base_url: http://127.0.0.1:8999
    assert:
      base_url: http://127.0.0.1:1
YAML
export QA_POSTER_TOKEN=qa-poster-only QA_READER_TOKEN=qa-reader-only
export ANTHROPIC_API_KEY=qa-dummy HEARSAY_DISTILL_QUIET=0
# This fixture supplies descriptive output only. The real distiller builds and
# validates the requests and writes L1; the assertion worker needs no model call.
cat > /tmp/model-fixture.py <<'PY'
import json
from http.server import BaseHTTPRequestHandler, HTTPServer
class Fixture(BaseHTTPRequestHandler):
    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert self.path == '/v1/messages'
        tool = request['tools'][0]['name']
        assert tool == 'l1_issue'
        response = {'id':'qa-fixture','type':'message','role':'assistant',
                    'model':request['model'],'stop_reason':'tool_use',
                    'usage':{'input_tokens':1,'output_tokens':1},
                    'content':[{'type':'tool_use','id':'qa-tool','name':tool,
                                'input':{'summary':'Delivery reliability work is planned.',
                                         'outcome_kind':'none'}}]}
        raw = json.dumps(response).encode()
        self.send_response(200)
        self.send_header('Content-Type','application/json')
        self.end_headers()
        self.wfile.write(raw)
HTTPServer(('127.0.0.1',8999),Fixture).serve_forever()
PY
python3 /tmp/model-fixture.py > /tmp/model-fixture.log 2>&1 &
pids="$pids $!"
for service in connectors distiller assert-worker api; do
  hearsay "$service" --config /tmp/work.yaml > "/tmp/work-$service.log" 2>&1 &
  pids="$pids $!"
done
for port in 8080 8081 8082 8083; do
  i=0
  until curl -fsS "http://127.0.0.1:$port/readyz" > /dev/null 2>&1; do
    i=$((i+1))
    if [ "$i" -gt 30 ]; then cat /tmp/work-*.log; exit 1; fi
    sleep 1
  done
done
echo 'PASS: work source, poster and four shipped services started'
project='{"kind":"project","project":"retry","name":"Resilient delivery","author":"reader","created_at":"2026-10-01T12:00:00Z"}'
stream='{"kind":"workstream","project":"retry","workstream":"policy","name":"Retry policy","created_at":"2026-10-01T13:00:00Z"}'
status=$(curl -sS -o /tmp/refused.json -w '%{http_code}' -H 'Content-Type: application/json' -d "$project" http://127.0.0.1:8081/hooks/projects)
[ "$status" = 401 ] || { cat /tmp/refused.json; exit 1; }
echo 'PASS: unauthenticated post refused with 401'
for request in "$project" "$stream"; do
  status=$(curl -sS -o /tmp/posted.json -w '%{http_code}' -H 'Content-Type: application/json' -H "Authorization: Bearer $QA_POSTER_TOKEN" -d "$request" http://127.0.0.1:8081/hooks/projects)
  [ "$status" = 202 ] || { cat /tmp/posted.json; exit 1; }
  jq -e '.id | startswith("evt:projects:")' /tmp/posted.json > /dev/null
done
echo 'PASS: authenticated project and workstream posts accepted with 202'
for artifact in retry 'retry#policy'; do
  i=0
  until curl -fsS -H 'Content-Type: application/json' -H 'Hearsay-Principal: reader' -H "Authorization: Bearer $QA_READER_TOKEN" -d "{\"id\":\"l1:projects:$artifact\"}" http://127.0.0.1:8080/v1/get_l1 > /tmp/read.json 2>/dev/null; do
    i=$((i+1)); if [ "$i" -gt 30 ]; then cat /tmp/work-*.log /tmp/model-fixture.log; exit 1; fi
    sleep 1
  done
  jq -e --arg artifact "$artifact" '.source.native_id == $artifact and .body.summary == "Delivery reliability work is planned."' /tmp/read.json > /dev/null
  echo "PASS: API read-back of l1:projects:$artifact"
done
# The follower may trail distillation; wait for the actual entity discovery.
i=0
until curl -fsS -H 'Content-Type: application/json' -H 'Hearsay-Principal: reader' -H "Authorization: Bearer $QA_READER_TOKEN" -d '{"text":"Retry policy"}' http://127.0.0.1:8080/v1/resolve | jq -e '.entities[] | select(.id == "work:projects:retry#policy")' > /tmp/entity.json; do
  i=$((i+1)); if [ "$i" -gt 30 ]; then cat /tmp/work-*.log; exit 1; fi
  sleep 1
done
cat /tmp/entity.json
echo 'PASS: API resolves the posted workstream entity'
