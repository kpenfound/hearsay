# 12. A GitHub review is versioned by a hash of its state and body

- Status: accepted
- Date: 2026-09-14

## Context

The GitHub table in [docs/connector-contract.md](../connector-contract.md) gave a
pull request review the native id `acme/api#31:review:77`, with no revision
token, because "a submitted review does not change". It does: its body can be
edited, and it can be dismissed, which moves its `state` to `DISMISSED`. GitHub
moves no version field for either — a review has `submitted_at` and no
`updated_at`.

The connector only emitted a review on `pull_request_review.submitted`, so a
webhook never collided. A backfill does: after a cleared cursor, or for a new
source over data already in L0, it reads the review's current body and state and
emits them under the id L0 already holds with the old content. `l0.Store.Append`
refuses that with `ErrRewrite`, and the runtime retries the backfill call for
ever (issue #73). The edit or dismissal itself was never ingested either.

The contract's own answer for a source without a usable version is to hash the
content into the revision token.

## Decision

A review's native id is `acme/api#31:review:77@<token>`, where the token is the
first 16 hex digits of the SHA-256 of the review's **lower-case state, a newline,
and its body**. `payload.revision.token` is that token, and `edited_at` is unset.

- **State and body are what change.** They are the only fields of a submitted
  review GitHub lets change; the author, commit and `submitted_at` do not. The
  state is lower-cased because REST spells it `APPROVED` and a webhook
  `approved`, and the two encodings must give one event id. A state contains no
  newline, so no state and body can be spelled as another pair.
- **16 hex digits** is 64 bits, which only has to tell apart the revisions of
  one review. A full digest would make every review id 48 characters longer for
  nothing.
- **No `edited_at`.** GitHub gives no time for an edit or a dismissal, and the
  delivery time would differ between a webhook and a backfill of the same
  content. Ingest order decides which revision is current, as the contract says
  for a source that gives no time.
- **The webhook emits `edited` and `dismissed`** as well as `submitted`, so a
  change reaches L0 when it happens and not only on the next backfill.
- In a private repository the token composes as every content token does:
  `<hash>+perm:private`.

## Alternatives considered

- **Skip a review L0 already holds.** Makes the backfill stop failing, but the
  connector would need to read L0, which the contract keeps it away from, and
  an edit or dismissal would never be ingested.
- **A token of the delivery or backfill time.** Not a function of the content:
  the same review read twice would be two revisions, and a webhook and a backfill
  would never deduplicate.
- **Hash the whole native payload and text.** Covers fields that cannot change
  and ties the id to the connector's own encoding choices, so an unrelated change
  to the payload would re-version every review.
- **Keep the bare id for a review that was never edited.** The connector cannot
  know what a review's original content was, so it cannot tell which reviews
  those are.
- **Edit the contract's table in place.** A different id format is a design
  change the contract says needs an ADR.

## Consequences

- An edited or dismissed review is a new revision of the same artifact, and a
  backfill over one succeeds.
- Every review is ingested once more under the new id the first time a source
  that emitted the old format backfills or receives a delivery for it; that
  revision's content is the same, and it becomes current by ingest order.
- A webhook delivery that arrives after a newer one for the same review, with
  content L0 has not seen, becomes current by ingest order although it is older.
  GitHub delivers a review's changes seconds apart and a backfill re-reads the
  current content, so this is left to the next backfill.
- A review edited back to content it had before gets that earlier revision's
  id again, which deduplicates, so the revision in between stays current until
  the review changes to something L0 has not seen. GitHub gives nothing
  monotonic to put in the token instead; accepted as rare, and harmless for a
  dismissal, which cannot be undone.
