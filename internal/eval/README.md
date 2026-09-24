# internal/eval

The evaluation metrics (docs/design.md#evaluation) that `hearsay eval` prints:
whether distillation is trustworthy rather than just confident.

**Belongs here:** turning what the layers recorded into a `Report` — one
section per metric, counts, durations, rates and scope and topic ids — and
printing it as text and as the stable JSON.

**Does not belong here:** SQL over a layer's tables, and the rule for where a
topic stands. Both are the layer's: time to ratification is replayed by
`l2.Store.Ratifications` through `l2.Stand`, and the rate reads
`l2.Store.Operations` and `l2.Store.TopicsOpened`. A new metric adds a
section to `Report` and reads what it needs through the owning package.

## Things to know before changing it

- **Aggregates only.** Nothing here reads or prints an L0 payload, L1 text, a
  stance's position or a topic's name, so the command needs no principal and
  no access check. A field that would carry one does not belong in `Report`.
- **The JSON is a contract.** Scripts read `hearsay eval --json`; add fields,
  never rename or remove them. A ratio with nothing to divide by is `null`,
  not `0`.
- **The window is half-open**, `[since, until)`, and a topic's time to
  ratification counts in the window its clock started in. What has not
  happened by `until` is still open, whatever happened after.
