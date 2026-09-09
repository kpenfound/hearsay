# internal/config

Configuration for all four services: the process settings, and the
configuration repository they read at startup.

**Belongs here:** the config types, loading and merging (defaults, then file,
then environment, then flags), and validation that fails at startup rather than
on the first request. `Load` reads the GitOps directory — `sources/`, `scopes/`,
`principals/`, `code/`, `authority/` — or the single file that expands to it,
and reports every problem it finds rather than the first.

**Does not belong here:** what the configuration is *for*. Identity resolution
is `internal/principal`, the connector runtime is `internal/connector`, and
deciding a stance's tier is the assertion worker. This package parses, checks
and hands over shapes those packages already take: sources come out as
`[]connector.SourceConfig`, and authority as a `Policy` per scope that answers
"does this outrank that" and "does this ratify" rather than as fields to
interpret.

**Three rules the code depends on and a reader would not guess:**

- A list that is absent and a list that is present but empty mean different
  things in an authority policy. Absent inherits; empty inherits nothing. That
  is why the schema types in `schema.go` keep the YAML shape and the types in
  `repo.go` keep the parsed one, rather than decoding straight into the second.
- A file that does not parse stops validation. What the file was meant to say is
  unknown, so every reference into it would be reported as missing.
- An object that failed validation is still put in the `Repo`, so that one
  mistake is reported once rather than once per thing that points at it — except
  when the id itself is what failed, and then it is held out, because there is
  nothing of that name for a reference to resolve to and saying so is the second
  half of the same mistake rather than a new one. `loader.claimID` is the rule; a
  duplicate id is not covered by it, because something of that name does exist.

The format is [docs/config.md](../../docs/config.md), the reasons are
[ADR-0009](../../docs/adr/0009-configuration-as-a-gitops-directory.md), and the
complete example in the first is loaded by the tests here, both ways round, so
the documentation cannot drift from the loader.

See [docs/design.md](../../docs/design.md#configuration).
