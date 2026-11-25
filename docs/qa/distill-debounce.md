# Distill debounce QA

Run the script against the image built from the working tree, with a throwaway
Postgres beside it:

```sh
dagger api call hearsay qa --script "$(cat docs/qa/distill-debounce.sh)"
```

The script migrates the disposable database and checks that both `distiller`
and `all` refuse `HEARSAY_DISTILL_QUIET=2s` with
`HEARSAY_DISTILL_MAX_DELAY=1s`. It then starts the distiller with valid
settings (`5s` quiet, `10s` maximum), inserts ten L0 revisions of one issue,
waits for the pump to consume them and stops the service before the job is
claimed. SQL assertions require all ten events to be consumed, exactly one
pending distill job, no attempts, and `run_after - enqueued_at = 5s`.

The model key is a dummy and both model endpoints point at an unreachable
loopback port, so the script makes no model or source call.

The last line of `qa` output is the script's exit code, not the outer Dagger
exit. A passing run ends with:

```text
PASS: ten revisions consumed, one pending distill job, configured quiet 5s
exit code: 0
```
