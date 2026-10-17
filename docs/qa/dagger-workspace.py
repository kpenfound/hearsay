#!/usr/bin/env python3
"""Run work QA or checks when the runner cannot discover the Dagger workspace.

Run under `dagger api with-session`; see work-updates.md for commands.
The upload, module serving and execution must share the same live session.
"""

import argparse
import base64
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.request


def query(document):
    token = base64.b64encode((os.environ['DAGGER_SESSION_TOKEN'] + ':').encode()).decode()
    request = urllib.request.Request(
        'http://127.0.0.1:' + os.environ['DAGGER_SESSION_PORT'] + '/query',
        data=json.dumps({'query': document}).encode(),
        headers={'Content-Type': 'application/json', 'Authorization': 'Basic ' + token},
    )
    try:
        with urllib.request.urlopen(request, timeout=1800) as response:
            data = json.load(response)
    except urllib.error.HTTPError as error:
        raise RuntimeError(error.read().decode()) from error
    if data.get('errors'):
        raise RuntimeError(json.dumps(data))
    return data['data']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument('--check', action='store_true', help='evaluate the full configured check collection')
    modes.add_argument('--work-tests', action='store_true', help='run work API integration tests on fresh migrations')
    args = parser.parse_args()
    ws = query(
        '{host{directory(path:' + json.dumps(os.getcwd()) + ',gitignore:false){asWorkspace{'
        'withConfigPaths(configFile:"dagger.toml",lockFile:"dagger.lock"){id}}}}}'
    )['host']['directory']['asWorkspace']['withConfigPaths']['id']
    print('workspace', ws, flush=True)
    node = '{node(id:' + json.dumps(ws) + '){... on Workspace{'
    module = query(node + 'moduleSource(path:".dagger/modules/hearsay"){asModule{id}}}}}')['node']['moduleSource']['asModule']['id']
    query('{node(id:' + json.dumps(module) + '){... on Module{serve}}}')
    if args.check:
        results = query(
            node + 'artifacts{filterTypes(types:["Check"]){values(maxConcurrency:4){value{__typename} error}}}}}}'
        )['node']['artifacts']['filterTypes']['values']
        print(json.dumps(results, indent=2), flush=True)
        if not results or any(result['error'] is not None for result in results):
            return 1
        print(f'PASS: all {len(results)} configured check results have error: null', flush=True)
    elif args.work_tests:
        source = query(node + 'directory(path:"/"){id}}}}')['node']['directory']['id']
        command = (
            'apk add --no-cache build-base && go run ./cmd/hearsay migrate up && '
            "go test -race -count=1 -v -tags=integration ./internal/service/api "
            "-run '^(TestPostedWork|TestWorkEntityVisibility)'"
        )
        result = query(
            '{hearsay{goTestBase(ws:' + json.dumps(ws) + '){'
            'withMountedDirectory(path:"/src",source:' + json.dumps(source) + '){'
            'withWorkdir(path:"/src"){withExec(args:' + json.dumps(['sh', '-c', command]) + '){stdout}}}}}}'
        )['hearsay']['goTestBase']['withMountedDirectory']['withWorkdir']['withExec']['stdout']
        print(result, flush=True)
    else:
        script = Path('docs/qa/work-updates.sh').read_text()
        result = query('{hearsay{qa(ws:' + json.dumps(ws) + ',script:' + json.dumps(script) + ')}}')['hearsay']['qa']
        print(result, flush=True)
        # QA returns script failures as text, even when the API request succeeds.
        if not result.rstrip().endswith('exit code: 0'):
            return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
