# 26. Artifacts may carry one of five core states, stated by their source

- Status: accepted
- Date: 2026-10-14

## Context

Readers need to distinguish proposed work, work in flight and finished work
across sources. Tracker workflows use different labels and transitions, and
documents can have a lifecycle too. Hearsay needs a common way to answer what
work is in flight while preserving what the source actually stated. An
artifact's distilled outcome describes its content; it does not establish
whether that artifact is being worked on or has been abandoned.

## Decision

An artifact may carry a source-stated state with one of five fixed core
values: `proposed` (described but not started), `started` (being worked on),
`blocked` (started but waiting on something), `completed` (done), or
`abandoned` (stopped and not done). The optional `native` label preserves the
source's wording verbatim, as 1–64 bytes of valid UTF-8.

The vocabulary is small so readers can compare and filter artifacts across
sources without learning every workflow. It is fixed, with no extensions,
so each value keeps one meaning across connectors. The native label keeps
source detail without enlarging that common vocabulary.

State lives in the L1 envelope because it is source metadata, copied
deterministically from the root event's current revision. It is independent
of the model-produced body: the model never sees, sets or changes it, and
the distiller prompt does not change. A state-only revision must refresh the
envelope even when the distilled text can be reused. Derived documents copy
their root's state through the shared structural path.

Sources map their own states onto the five values. Hearsay does not model
their state machines, transitions or workflow configuration: the source
knows those semantics and remains responsible for stating them. No state is
inferred from text, model output or a free-form tracker status. Absence stays
absence.

In L0, `payload.state` is optional and valid only on `issue`, `pull_request`
and `document` root events, including extension kinds whose `base_kind` is
one of those. Other kinds are validation errors when they carry state.
Transcripts remain ineligible, so meeting segments built from valid
transcript roots carry no state, through the same structural copy path.

## Alternatives considered

- **An extensible vocabulary or only native labels.** Every reader would
  need source-specific rules to decide what counts as work in flight.
- **Put state in the distilled body or infer it with the model.** A source
  fact would depend on model output and could drift when text is distilled
  again, even though the source had not changed its state.
- **Model each source's state machine in Hearsay.** Hearsay would have to
  track workflow configuration and transitions for each source to answer a
  question that needs only the five common values.

## Consequences

- Connectors can opt in without changing events that carry no state. This
  decision does not make existing connectors start mapping their states.
- A state change is a content change and requires a new revision. L1 copies
  the current state, stores it, and exposes it in document reads and filters.
- Entity readers take state from the subject document; no entity state is
  stored. Bundle output stays unchanged.
- The native label retains nuance that the five values intentionally omit.
  Mapping it correctly is the source's responsibility.
