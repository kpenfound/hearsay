#!/usr/bin/env python3
"""Run the state trial in Dagger's shipped-image QA sandbox, from repo root."""
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile
import urllib.request
import urllib.error

HERE = Path(__file__).resolve().parent


def query(document, variables=None):
    token = base64.b64encode((os.environ['DAGGER_SESSION_TOKEN'] + ':').encode()).decode()
    request = urllib.request.Request(
        'http://127.0.0.1:' + os.environ['DAGGER_SESSION_PORT'] + '/query',
        data=json.dumps({'query': document, 'variables': variables or {}}).encode(),
        headers={'Content-Type': 'application/json', 'Authorization': 'Basic ' + token})
    try:
        with urllib.request.urlopen(request, timeout=600) as response:
            result = json.load(response)
    except urllib.error.HTTPError as error:
        raise RuntimeError(error.read().decode()) from error
    if result.get('errors'):
        raise RuntimeError(result['errors'])
    return result['data']


if 'DAGGER_SESSION_PORT' not in os.environ:
    subprocess.run(['dagger', 'api', 'with-session', '--silent', 'python3', str(Path(__file__).resolve())], check=True)
else:
    with tempfile.TemporaryDirectory(prefix='hearsay-state-qa-') as scratch:
        helper = Path(scratch) / 'state-filter'
        env = dict(os.environ, CGO_ENABLED='0', GOOS='linux')
        subprocess.run(['go', 'build', '-ldflags', '-s -w', '-o', str(helper), str(HERE / 'filter.go')], env=env, check=True)
        # A static helper checks ListFor without a production endpoint or image
        # change. The API sends this script without a process argument limit.
        encoded = base64.b64encode(helper.read_bytes()).decode()
        script = "base64 -d > /tmp/state-filter <<'HELPER'\n" + encoded + "\nHELPER\nchmod +x /tmp/state-filter\n" + (HERE / 'trial.sh').read_text()
        workspace = query('{ host { directory(path: ".") { asWorkspace { id moduleSource(path: ".dagger/modules/hearsay") { asModule { serve(entrypoint: true) } } } } } }')
        workspace_id = workspace['host']['directory']['asWorkspace']['id']
        transcript = query('query($ws: ID!, $script: String!) { qa(ws: $ws, script: $script) }',
                           {'ws': workspace_id, 'script': script})['qa']
        print(transcript)
        if not transcript.rstrip().endswith('exit code: 0'):
            raise SystemExit('QA trial failed')
