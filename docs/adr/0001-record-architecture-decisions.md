# 1. Record architecture decisions in ADRs

- Status: accepted
- Date: 2026-09-08

## Context

Hearsay is being built one milestone at a time by people and agents who do not share
a memory. `docs/design.md` says what the system is; it does not say why the
implementation picked one tool over another, and it is a living document that gets
rewritten. Decisions recorded only in a pull request thread or an issue comment are
effectively lost: the next person re-argues them, and the answer drifts.

## Decision

Architecture decisions are recorded as numbered, dated files under `docs/adr/`.

- Filename: `NNNN-kebab-case-title.md`, `NNNN` zero-padded to four digits, assigned in
  order of merge. Take the next free number; if two branches collide, the second to
  merge renumbers.
- Every ADR has the sections in this file's shape: a title line, a `Status` and `Date`
  header, then **Context**, **Decision**, **Alternatives considered**, and
  **Consequences**.
- Status is one of `accepted`, `superseded by ADR-NNNN`, or `deprecated`. An accepted
  ADR is never edited to change its decision. It is superseded by a new ADR, and the
  old one gets a status line pointing at the new one. Fixing a typo or adding a link is
  fine.
- `docs/adr/README.md` is the index. Adding an ADR means adding its row.

An ADR is for decisions that constrain later work: something a future contributor could
plausibly do differently and would need a reason not to. Choosing a library because it
is the only one that does the job is not an ADR. Choosing between two that both work is.

## Alternatives considered

- **A single `docs/decisions.md`.** One file, appended to. Cheaper to skim, but it
  merges badly when several branches add decisions at once, and there is no stable
  anchor to cite from an issue or a code comment.
- **Decisions in `docs/design.md`.** The design doc describes the system as it is meant
  to work. Mixing "we chose goose over golang-migrate" into it buries the design under
  tooling detail, and the design doc gets rewritten as the product changes, which would
  quietly erase the record.
- **No ADRs; rely on git history and PR descriptions.** Git says what changed, rarely
  why, and PR threads are not readable in order two years later.
- **A tool (adr-tools, log4brains).** A dependency and a workflow to learn for something
  that is a file with five headings.

## Consequences

- `docs/adr/README.md` must be updated in the same commit as a new ADR, or the index
  goes stale immediately.
- Superseding rather than editing means the directory only grows, and reading the
  current state means reading the index rather than every file. The index carries the
  status, so it stays a single lookup.
- Reviewers have somewhere to send a decision that is being made implicitly in a pull
  request.
