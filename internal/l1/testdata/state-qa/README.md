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
endpoint, connector, prompt, bundle or image is changed by the trial. The
local HTTP model fixture supplies a recorded body and refuses a request that
exposes the source's state label. No live model or tracker is contacted.

The runner checks the QA transcript's exit code, because `qa` returns a
script's failure as text. `DAGGER_ENGINE` selects an engine when needed; the
helper targets Linux and respects `GOARCH` if the engine architecture differs
from the local Go toolchain.

The successful transcript includes:

```text
get_l1: l1:qa:ENG#QA-1 {'core': 'started', 'native': 'In review'}
search: l1:qa:ENG#QA-1 {'core': 'started', 'native': 'In review'}
ListFor(started): 1 documents
ListFor(completed): 0 documents
exit code: 0
```
