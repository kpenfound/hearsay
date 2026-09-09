# internal/config

Configuration for all four services.

**Belongs here:** the config types, loading and merging (defaults, then file,
then environment, then flags), and validation that fails at startup rather than
on the first request.

**Does not belong here:** the contents of the GitOps config repo — `sources/`,
`scopes/`, `principals/`, `code/`, `authority/` — beyond the types needed to
read it. Identity mapping lives in `internal/principal`.

Today this package carries only the logging settings the scaffold uses. The real
format is designed in issue #5 and built in #37; a single file has to cover a
GitHub-plus-Slack team, because adoption dies at the config step.

See [docs/design.md](../../docs/design.md#configuration).
