# internal/config

Configuration for all four services: the process settings, and the
configuration repository they read at startup.

**Belongs here:** the config types, loading and merging (defaults, then file,
then environment, then flags), and validation that fails at startup rather than
on the first request. `Load` reads the GitOps directory — `sources/`, `scopes/`,
`principals/`, `code/`, `authority/`, `llm/` — or the single file that expands
to it, and reports every problem it finds rather than the first.

**Does not belong here:** what the configuration is *for*. Identity resolution
is `internal/principal`, the connector runtime is `internal/connector`, and
deciding a stance's tier is the assertion worker. This package parses, checks
and hands over shapes those packages already take: sources come out as
`[]connector.SourceConfig`, the model tiers as an `llm.Config` the registry
builds from, and authority as a `Policy` per scope that answers "does this
outrank that" and "does this ratify" rather than as fields to interpret.

**Four rules the code depends on and a reader would not guess:**

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

- The `llm/` section is merged onto the shipped defaults field by field, so a
  tier naming only a model keeps the default provider, and a repository with no
  `llm/` at all still has a working `distill` and `assert` tier — ADR-0005 puts
  the shipped model in the config defaults so that bumping it is one line. A
  tier is checked by `llm.TierConfig.Validate`, called from here rather than
  copied, so `hearsay config validate` and a service starting up cannot
  disagree about what a valid tier is; what this package adds is a file, a
  line, and the provider catalogue in `internal/llm/providers`.

The format is [docs/config.md](../../docs/config.md), the reasons are
[ADR-0009](../../docs/adr/0009-configuration-as-a-gitops-directory.md), and the
complete example in the first is loaded by the tests here, both ways round, so
the documentation cannot drift from the loader.

See [docs/design.md](../../docs/design.md#configuration).

`llm.boost` configures the separate typed judgment capability and is returned as
`Repo.Boost`. It has one `enabled` switch, default false, an explicit `model`,
optional `base_url`, `api_key_env` (default `HEARSAY_JEV_API_KEY`), transport
bounds (`timeout`, `total_timeout`, `max_retries`, `backoff`, `cooldown`,
`cooldown_failures`) and `assertion_job_budget` (default 5s, maximum 30s).
The job budget covers every optional call and wait for the entire assertion job.
`max_total_tokens` defaults to 64000 and `max_state_question_tokens` to 32000.
Either limit may be lowered; it cannot exceed the captured vendor limit. The
adapter conservatively counts serialized UTF-8 bytes as a token upper bound,
including schema overhead, and refuses oversize requests before network IO.
No credential is consulted by configuration validation. The optional capability
handles missing credentials by degrading while baseline services keep working.

`policies` is a map keyed by `stance_comparison`, `topic_matching`,
`topic_shortlist` or `outcome_classification`. Each entry requires `name`, a
pinned `model_version` and an explicit `threshold` between zero and one. No
policies ship by default. Unknown keys, including any per-integration enable
flags, are refused. A boost-only `llm/boost.yaml` may sit beside tier files;
configuring the boost in two files is refused rather than merged.
