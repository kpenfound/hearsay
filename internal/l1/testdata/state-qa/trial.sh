set -eu
apk add --no-cache python3 curl >/dev/null
export HEARSAY_TRACKER_TOKEN=qa-tracker-token HEARSAY_QA_TOKEN=qa-api-token ANTHROPIC_API_KEY=qa-local-fixture
mkdir -p /tmp/state-config
cat > /tmp/state-config/hearsay.yaml <<'YAML'
sources:
  - id: qa
    type: tracker
    containers: [ENG]
    settings:
      access:
        ENG: [{kind: public}]
    secrets:
      token: HEARSAY_TRACKER_TOKEN
scopes:
  - id: qa
    sources: [qa]
    tracker: {source: qa, project: ENG}
principals:
  - id: qa
    kind: human
    token_env: HEARSAY_QA_TOKEN
    scopes: ["*"]
    identities:
      - {source: qa, native_id: u1}
llm:
  tiers:
    distill:
      provider: anthropic
      model: qa-fixture
      base_url: http://127.0.0.1:8999
    assert:
      provider: anthropic
      model: qa-fixture
      base_url: http://127.0.0.1:8999
YAML
# A local recorded answer exercises the production provider adapter without
# calling a model provider. It verifies state is absent from the model input.
cat > /tmp/state-model.py <<'PY'
import json
from http.server import BaseHTTPRequestHandler, HTTPServer
class Fixture(BaseHTTPRequestHandler):
    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert 'In review' not in json.dumps(request)
        tool = request['tools'][0]['name']
        body = {'id':'qa-recorded','type':'message','role':'assistant','model':'qa-fixture',
                'content':[{'type':'tool_use','id':'qa-tool','name':tool,
                            'input':{'summary':'QA ticket for state propagation.','outcome_kind':'none'}}],
                'stop_reason':'tool_use','usage':{'input_tokens':10,'output_tokens':10}}
        encoded = json.dumps(body).encode()
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers(); self.wfile.write(encoded)
HTTPServer(('127.0.0.1',8999), Fixture).serve_forever()
PY
python3 /tmp/state-model.py > /tmp/state-model.log 2>&1 &
model_pid=$!
hearsay all --config /tmp/state-config > /tmp/state-services.log 2>&1 &
service_pid=$!
trap 'kill "$service_pid" "$model_pid" 2>/dev/null || true' EXIT
for attempt in $(seq 1 60); do
  if curl -fsS http://127.0.0.1:8081/readyz >/dev/null 2>&1 && curl -fsS http://127.0.0.1:8080/readyz >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS -H 'Authorization: Bearer qa-tracker-token' -H 'Content-Type: application/json' \
  -d '{"kind":"ticket","project":"ENG","id":"QA-1","title":"QA state ticket","body":"Verify deterministic artifact state propagation.","state":"started","status":"In review","author":{"id":"u1","handle":"qa"},"created_at":"2026-10-14T10:00:00Z","updated_at":"2026-10-14T10:00:00Z"}' \
  http://127.0.0.1:8081/hooks/qa
for attempt in $(seq 1 60); do
  if curl -fsS -H 'Authorization: Bearer qa-api-token' -H 'Hearsay-Principal: qa' -H 'Content-Type: application/json' \
      -d '{"id":"l1:qa:ENG#QA-1"}' http://127.0.0.1:8080/v1/get_l1 > /tmp/state-document.json; then break; fi
  sleep 1
done
curl -fsS -H 'Authorization: Bearer qa-api-token' -H 'Hearsay-Principal: qa' -H 'Content-Type: application/json' \
  -d '{"query":"QA state ticket","scope":"tracker:qa:ENG#QA-1"}' http://127.0.0.1:8080/v1/search > /tmp/state-search.json
python3 - <<'PY'
import json
expected = {'core':'started','native':'In review'}
with open('/tmp/state-document.json') as f: document = json.load(f)
assert document['state'] == expected, document
print('get_l1:', document['id'], document['state'])
with open('/tmp/state-search.json') as f: search = json.load(f)
hit = next(d for d in search['documents'] if d['id'] == document['id'])
assert hit['state'] == expected, hit
print('search:', hit['id'], hit['state'])
PY
/tmp/state-filter
