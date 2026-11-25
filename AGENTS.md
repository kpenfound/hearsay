# Working on Hearsay

## Scope

Hearsay ingests team communication, distills it into four layers that keep their provenance, and serves scoped, permission-filtered context bundles to coding agents. One binary runs four services: connectors, distiller, assertion worker and API.

Packages under `cmd` and `internal` say what belongs in them in a README or a package comment. Read it for the package you are about to touch. [CONTRIBUTING.md](CONTRIBUTING.md) has the commands, the Dagger checks and how to try a change; [docs/config.md](docs/config.md) is the configuration schema and [docs/deploy.md](docs/deploy.md) the deployment guide.

## Design

[docs/design.md](docs/design.md) is the source of truth. When a change conflicts with it, raise the conflict instead of silently changing product scope.

[docs/connector-contract.md](docs/connector-contract.md) and `internal/connector` change together or not at all.

## Binding rules

- Errors are returned and wrapped with `fmt.Errorf("...: %w", err)`, and the message says what failed. Compare with `errors.Is` and `errors.As`. Nothing panics outside `main`.
- Context is the first parameter of anything that does IO, is threaded through, and is never stored in a struct. Every goroutine has an owner that can cancel it and wait for it.
- Logging is `log/slog` taken from the context, never a package-level `slog.Info`. Never log L0 payloads, L1 `text` or `raw_text`, prompts or completions at `info` or above: log ids. Never log credentials, tokens or webhook signatures at any level. A package whose own rule is stricter keeps it. This is access control, not style; [internal/telemetry](internal/telemetry/README.md) has the field names.
- No test calls a model provider or a live source. Model calls in tests go through the fake in `internal/llm` with a recorded fixture.
- Migrations are plain SQL in `internal/db/migrations/`. A merged migration is never edited. They are applied by `hearsay migrate` as a deploy job and never at service startup; `hearsay all` is the development exception.
- The subcommand names, the model tier names (`distill`, `assert`, `embed`) and the `Run(ctx, cfg, deps) error` signature are contracts. Changing one is a breaking change to call out in the pull request description.
- Tests are table-driven with named cases and use the package's exported surface. Every bug fix arrives with the test that fails without it.

## Checks

- Run focused checks for what you changed, for example `dagger check --go --test --go-package=internal/config`. `dagger check -l` lists them. The check service runs the full suite.
- Never run `go test` on the host. Tests run only inside Dagger. `gofmt`, `go build` and `go vet` are fine on the host.
- Tests read `docs/config.md`, `docs/connector-contract.md` and `cmd/hearsay/README.md` through `includeExtraFiles` in `dagger.toml`. A test that opens another file outside `testdata/` adds it there.
- To try a change end to end, use `dagger api call hearsay qa --script '...'`. It runs the shipped image built from the working tree with Postgres beside it.
- Report the checks you ran and their results. If validation is blocked, state the exact blocker.

## Comments

Follow the no-comments and no-deltas skills. The one exception: accurate doc comments on exported identifiers and packages, including under `internal/`. Directives such as `//go:build`, `//go:embed` and `//nolint` are not comments.

YAML, TOML, Dang and shell files carry no comments either. The exceptions are the comments that document a setting in `deploy/compose/env.example` and the Helm chart's `values.yaml`. Docs and READMEs describe current behaviour only.

## Repo-specific skills

- [.claude/skills/dagger](.claude/skills/dagger/SKILL.md) is vendored. Load it for Dagger workspace, module and check work. Do not edit it. Where it allows a native `go test`, the rule above wins.
