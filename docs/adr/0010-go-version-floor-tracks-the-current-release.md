# 10. The Go version floor tracks the current release

- Status: accepted
- Date: 2026-09-09
- Supersedes: the version clause of [ADR-0002](0002-go-for-language-and-runtime.md)

## Context

ADR-0002 chose Go, and in the same breath set a rule for which Go:

> `go.mod` declares the minimum version the code requires. It starts at **Go
> 1.24** and is raised deliberately, in its own commit, when something needs a
> newer feature.

That is a library's rule. A library declares the oldest toolchain that compiles
it, because its `go` directive is a promise to everyone who imports it: raise
the floor and you drop consumers who cannot move yet.

Hearsay is not imported. It is four processes in one statically linked binary,
shipped as a container image and self-hosted per organization (ADR-0002,
ADR-0003). Nobody's build is constrained by our `go` directive. An operator runs
the image; a contributor runs `dagger check`.

So the rule bought nothing, and it did not survive the first commit that had an
opinion. When the scaffold landed (#34), `go.mod` was set to `go 1.27.1` — the
current release — on a person's direction that a new project should start on a
supported release rather than one three behind. No 1.27 feature is used. Within
a day of ADR-0002 being written, both its number and its stated reason for
changing that number were out of step with the repository.

The deviation was recorded in CONTRIBUTING.md, which is where a contributor
reads it, but CLAUDE.md sends anyone asking *why* to `docs/adr/`, and ADR-0002
is what they find. That is issue #41.

There is a second reason to change the rule rather than change `go.mod` back.
A declared floor of 1.24 was untested prose: `.dagger/main.go` reads the `go`
directive out of `go.mod` and builds every container from `golang:<that
version>`, so every check ran on 1.27.1 and only on 1.27.1. The day someone
wrote a 1.27-only construct, no gate would have noticed and the floor would have
been wrong without anyone touching it. A number no gate exercises is a claim,
not a constraint.

## Decision

The Go version floor tracks the current Go release, rather than the oldest
release that compiles. This replaces ADR-0002's version clause; the rest of
ADR-0002 — Go as the language, the standard library as the default, no cgo, one
static binary — stands unchanged.

- **`go.mod` declares a current Go release, patch included.** `go 1.27.1` today.
  Hearsay supports that release and newer, and nothing older is tested.
- **It is raised because a newer release exists**, not because the code needs a
  feature from it. A new minor release is reason enough on its own. Patch
  releases are picked up when one matters or when the line is being touched
  anyway; a floor a patch or two behind the newest is not a defect to file.
- **Raising it stays what ADR-0002 said it was**: deliberate, and its own
  commit, doing nothing else. That commit is where a new toolchain's stricter
  vet or lint shows up, which is the point of keeping it separate.
- **`go.mod` is the only place the version is written.** `.dagger/main.go`
  derives the toolchain from the `go` directive; CONTRIBUTING.md names the file
  rather than the number. Raising the floor is a one-line diff with nothing else
  to keep in step.

## Alternatives considered

- **Keep ADR-0002's clause and lower `go.mod` back to 1.24.** Honest about what
  the code actually requires, and correct for a module other people import.
  Rejected on both halves: no one imports this module, so the floor buys nobody
  compatibility, and no gate builds at it, so it would go stale silently. It
  also reverses a person's direction on grounds — library compatibility — that
  do not apply here.
- **Leave ADR-0002 alone and let CONTRIBUTING.md carry the floor**, the other
  option offered in #41. CONTRIBUTING.md is where a contributor reads the
  prerequisite and it was accurate. Rejected because the "why" question is
  routed to `docs/adr/` by both CLAUDE.md and CONTRIBUTING.md, and the answer
  found there contradicted `go.mod`. A decision that lives only as prose
  explaining why an ADR is overridden is the drift ADR-0001 exists to stop.
- **Edit ADR-0002's version clause in place.** One file changed and no reader
  confused. Forbidden by ADR-0001: an accepted ADR records what was decided on
  its date, and is superseded rather than rewritten.
- **A `toolchain` directive instead of a patch-level `go` directive.**
  `go 1.24` plus `toolchain go1.27.1` separates the language version the code
  needs from the toolchain the repository builds with, and both statements would
  be true. Rejected: it is two numbers to maintain instead of one, the language
  version stays the untested claim it already was, and no consumer reads the
  `go` line for compatibility. One number every gate actually runs on is worth
  more than an accurate one nobody checks.
- **Floor at the previous release (N-1).** Go supports the two newest minor
  releases, so N-1 is the conservative choice for a project that has to
  accommodate other people's toolchains. Hearsay does not, and N-1 has the same
  untested-claim problem one release smaller.

## Consequences

- Raising the floor raises everything at once. `.dagger/main.go`'s `goVersion`
  reads the `go` directive and pulls `golang:<version>`, so lint, unit tests,
  integration tests, the binary and the image all move on the same one-line
  commit, and `dagger check` is what says whether the new release breaks
  Hearsay.
- The declared floor is exercised rather than asserted: every check builds and
  runs at exactly the version `go.mod` names.
- Somebody still has to do it. The floor does not follow Go automatically, and
  nothing breaks while it lags — a stale floor is an old toolchain, not a broken
  build.
- A contributor on an older Go gets a toolchain download on the first build
  rather than a compile error, because `GOTOOLCHAIN` is left at its default
  `auto`. That needs the network once, which CONTRIBUTING.md says.
- CONTRIBUTING.md no longer states a Go version. Its prerequisite points at
  `go.mod`, matching what it already said about the linter: the toolchain
  versions come from the repository, not from that document.
- ADR-0002 keeps its `accepted` status and its text. Only its version clause is
  replaced, so marking the whole ADR superseded would be wrong — Go is still the
  language for the reasons it gives. The partial supersession is carried by the
  index in `docs/adr/README.md`, which ADR-0001 makes the single lookup for an
  ADR's current standing.
- If Hearsay ever publishes an importable Go module — an SDK for the bundle API,
  say — its `go` directive becomes a compatibility promise to consumers, and
  this decision does not apply to it. That module gets the library rule and, if
  it is a module of its own, its own floor.
