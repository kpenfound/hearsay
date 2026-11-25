# Operator path QA

Run the script against the image built from the working tree, with a throwaway
Postgres beside it:

```sh
dagger api call hearsay qa --script "$(cat docs/qa/operator-path.sh)"
```

The script unsets the database URL and every source credential, then runs
`hearsay init --no-input --force` for a GitHub, Discord and Drive team. It
asserts that:

- the generated configuration validates as root and as `nobody`, which is the
  user the image runs as;
- `hearsay.yaml` is mode 0644 and the env file is mode 0600;
- a second `init` without `--force` refuses both existing files, says nothing
  was written, and leaves both byte-identical.

The script makes no source, model or database call. A passing run ends with
`operator rehearsal: ok` and `exit code: 0`; the last line is the script's exit
code, not the outer Dagger exit.
