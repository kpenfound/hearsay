# 9. Configuration is a GitOps directory of YAML, read at startup

- Status: accepted
- Date: 2026-09-09

## Context

The design doc says configuration is "a repo, applied like GitOps", with five
directories — `sources/`, `scopes/`, `principals/`, `code/`, `authority/` — and
that "adoption dies at the config step if identity mapping and connector
onboarding are not near-zero effort" (docs/design.md#configuration). Both halves
of that have to be true at once: a shape that scales to a real organisation, and
a first day that is one file.

Configuration is also the only place a person tells Hearsay who may decide
things. Authority is per scope, and getting it wrong means agents either act on
things nobody agreed or ask about things everybody did.

Three decisions were open: what the files look like, how they are read, and what
happens when they change while processes are running.

## Decision

**A directory of YAML, and a single file that expands to it.** The five
directories are the canonical form. A single `hearsay.yaml` with one key per
directory is the same configuration with the lists in one place, and the loader
produces the same result from either. Nothing is only expressible in one form,
so a team can split the file the day it becomes awkward and change nothing else.
The schema is docs/config.md.

**YAML, with `go.yaml.in/yaml/v3`.** This is the first third-party dependency,
and the standard library is otherwise the default (ADR-0002). Configuration here
is written by hand and reviewed in pull requests, which rules out JSON: no
comments, and no way to explain a channel id in the line that names it. The
library earns its place beyond parsing — it rejects unknown fields, which is
what turns a misspelled key from silence into an error, and it reports the line
a problem is on, which is most of what makes the errors specific. The module is
the YAML organisation's maintained continuation of `gopkg.in/yaml.v3`, which is
archived.

**Read at startup, once. No polling, no watching, no reload signal.** A
configuration change is a deploy, the way a migration is a deploy job
(ADR-0006). Three reasons:

- Half of a configuration change is not applicable in place. Removing a source
  has to stop a connector, its poll loop and its cursor; changing a scope's
  authority mid-batch would judge two stances in one queue run by two policies,
  and the assertion worker is serialized per scope precisely so that cannot
  happen (ADR-0007).
- A reload path is a second way for a process to fail, at the worst moment: a
  running deployment picking up a bad configuration file is a harder failure to
  reason about than a container that will not start.
- It is checkable. Every process logs a `config_digest` at startup and
  `hearsay config validate` prints the same digest, so "has this been rolled
  yet" has an answer rather than an assumption.

**`hearsay config validate` reports everything, not the first thing.** It takes
a path, loads it exactly as a service would, and prints every problem with a
file, a line and a field. It needs no database, no network and no credentials,
so it runs in CI on the configuration repository as a gate on a pull request.

**The loader lives in `internal/config` and produces what consumers already
take.** Sources come out as `[]connector.SourceConfig`, which is what the
connector runtime and the ingest allowlist consume, so there is no second shape
to keep in step. Authority comes out as a `Policy` per scope with the questions
the assertion worker asks — does this artifact outrank that one, does this
ratify — already answered, rather than as fields for it to interpret.

To make that possible, `internal/telemetry` stopped taking `config.Log` and
takes the two values instead: `internal/config` reads configuration for every
package including the ones telemetry is used from, so a utility that config
cannot import is a utility in the wrong place.

## Alternatives considered

**JSON, or a Go struct with environment variables.** No comments in JSON, and
environment variables cannot express a list of principals with several
identities each. Neither survives contact with the authority schema.

**TOML.** Comments and no dependency problem worse than YAML's, but the nesting
here — principals with lists of identities, scopes with lists of sources with
lists of containers — is where TOML gets hard to read, and it is the format
fewest of the tools around a configuration repository already speak.

**Directory form only.** Rejected against the design doc's own adoption
requirement. Five directories to hold four objects is what a team abandons at
the config step.

**Single file only, expanded by a command later.** It would have made the file
the canonical form and the directory a generated artifact, and generated
configuration in a GitOps repository is a thing to review that nobody wrote.

**Watching the configuration directory with a filesystem notification, or
polling it.** Deferred rather than rejected. What would have to be true first:
connectors that can be stopped and started individually, an assertion worker
that takes its policy per job rather than per process, and a bundle cache keyed
by the digest. That is a change to three services, and it belongs with them
rather than with the format.

**Authority as a property of sources rather than a per-scope policy.** Simpler,
and wrong for the case that motivates the feature: the team whose decisions land
in meetings and the team whose decisions land in merged pull requests are in one
deployment, and one ranking for both makes one of them wrong.

## Consequences

- The first dependency is in. `go mod tidy` now matters, CI caches modules, and
  the "no third-party dependencies" line in CONTRIBUTING.md is gone.
- Changing configuration requires a rollout. On a deployment where that is slow,
  it is slow; the digest at least makes it visible.
- The single-file form is a second parsing path, and it is tested by loading the
  example in docs/config.md both ways and comparing the results, so the two
  cannot drift.
- Config validation happens twice: at `config validate` time and at startup.
  That is deliberate — the second one is what actually protects a deployment,
  and the first is what keeps a bad change from being merged.
- The authority schema answers one of the design doc's open questions. The other
  one, how `part_of` edges are built and who may restructure them, is only
  partly touched: `code/` seeds the hierarchy, and nothing yet says who may
  change it afterwards.
