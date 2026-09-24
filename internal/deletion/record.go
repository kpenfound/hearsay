package deletion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// A deletion's rebuild status.
const (
	// StatusRebuilding is a deletion with a document not rebuilt yet, or a
	// `distill` or `assert` job on what it rebuilt still to run.
	StatusRebuilding = "rebuilding"
	// StatusComplete is a deletion every rebuild job of which has finished.
	StatusComplete = "complete"
)

// OutcomePending is a queued document the distiller has not rebuilt yet.
const OutcomePending = "pending"

// ErrNotFound is returned by [Show] for an id no deletion has.
var ErrNotFound = errors.New("no such deletion")

// Summary is one line of `hearsay delete list`.
type Summary struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Operator string    `json:"operator"`
	Reason   string    `json:"reason"`
	Selector Selector  `json:"selector"`
	Status   string    `json:"status"`
}

// Rebuilt is what became of one L1 document a deletion queued.
type Rebuilt struct {
	ID string `json:"id"`
	// Outcome is l0.RebuiltRedistilled, l0.RebuiltDeleted or [OutcomePending].
	Outcome string `json:"outcome"`
	// At is when it was rebuilt, and nil while it is pending.
	At *time.Time `json:"at,omitempty"`
}

// Record is the whole of a deletion: what it covered and what it rebuilt.
// Every slice is sorted and non-nil.
type Record struct {
	Summary
	// Retraction is the `deletion` event written under source `hearsay`.
	Retraction string `json:"retraction"`
	// Replays counts connector re-emissions of a covered event, dropped.
	Replays int64 `json:"replays"`
	// Events are the L0 events redacted.
	Events []string `json:"events"`
	// Documents are the L1 documents queued, and what became of each.
	Documents []Rebuilt `json:"documents"`
	// Stances are the stances superseded after the deletion rebuilt their
	// evidence, whose position was redacted.
	Stances []string `json:"stances"`
	// Topics are the topics whose name was redacted because nothing but the
	// deleted content supported them.
	Topics []string `json:"topics"`
}

type row struct {
	summary    Summary
	selector   []byte
	documents  []string
	retraction string
	replays    int64
	events     []string
}

const recordColumns = `id, deleted_at, operator, reason, selector, documents, retraction, replays, events`

func scanRow(rows pgx.CollectableRow) (row, error) {
	var r row
	err := rows.Scan(&r.summary.ID, &r.summary.Time, &r.summary.Operator, &r.summary.Reason,
		&r.selector, &r.documents, &r.retraction, &r.replays, &r.events)
	if err != nil {
		return r, err
	}
	r.summary.Time = r.summary.Time.UTC()
	if err := json.Unmarshal(r.selector, &r.summary.Selector); err != nil {
		return r, fmt.Errorf("decoding the selector of deletion %s: %w", r.summary.ID, err)
	}
	return r, nil
}

// List is every deletion, newest first, with its rebuild status.
func List(ctx context.Context, pool *pgxpool.Pool) ([]Summary, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT `+recordColumns+` FROM l0_deletions ORDER BY deleted_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing deletions: %w", err)
	}
	found, err := pgx.CollectRows(rows, scanRow)
	if err != nil {
		return nil, fmt.Errorf("listing deletions: %w", err)
	}
	out := make([]Summary, 0, len(found))
	for _, r := range found {
		docs, err := rebuilds(ctx, tx, r.summary.ID, r.documents)
		if err != nil {
			return nil, err
		}
		if r.summary.Status, err = status(ctx, tx, docs); err != nil {
			return nil, err
		}
		out = append(out, r.summary)
	}
	return out, nil
}

// Show is one deletion's full record, read in one snapshot.
func Show(ctx context.Context, pool *pgxpool.Pool, id string) (Record, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT `+recordColumns+` FROM l0_deletions WHERE id = $1`, id)
	if err != nil {
		return Record{}, fmt.Errorf("reading deletion %s: %w", id, err)
	}
	r, err := pgx.CollectExactlyOneRow(rows, scanRow)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Record{}, fmt.Errorf("reading deletion %s: %w", id, err)
	}
	rec := Record{Summary: r.summary, Retraction: r.retraction, Replays: r.replays, Events: r.events}
	if rec.Documents, err = rebuilds(ctx, tx, id, r.documents); err != nil {
		return Record{}, err
	}
	if rec.Status, err = status(ctx, tx, rec.Documents); err != nil {
		return Record{}, err
	}
	if rec.Stances, err = queryIDs(ctx, tx, `SELECT id FROM l2_stances WHERE redacted_by = $1 ORDER BY id`, id); err != nil {
		return Record{}, fmt.Errorf("reading the stances deletion %s redacted: %w", id, err)
	}
	if rec.Topics, err = queryIDs(ctx, tx, `SELECT id FROM l2_topics WHERE redacted_by = $1 ORDER BY id`, id); err != nil {
		return Record{}, fmt.Errorf("reading the topics deletion %s redacted: %w", id, err)
	}
	if rec.Events == nil {
		rec.Events = []string{}
	}
	return rec, nil
}

// rebuilds is what became of each document a deletion queued, in id order.
func rebuilds(ctx context.Context, tx pgx.Tx, id string, documents []string) ([]Rebuilt, error) {
	rows, err := tx.Query(ctx, `
SELECT doc, coalesce(r.outcome, $3), r.rebuilt_at
  FROM unnest($2::text[]) AS doc
  LEFT JOIN l0_deletion_rebuilds r ON r.deletion = $1 AND r.document = doc
 ORDER BY doc`, id, documents, OutcomePending)
	if err != nil {
		return nil, fmt.Errorf("reading what deletion %s rebuilt: %w", id, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Rebuilt, error) {
		var r Rebuilt
		err := row.Scan(&r.ID, &r.Outcome, &r.At)
		if r.At != nil {
			at := r.At.UTC()
			r.At = &at
		}
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading what deletion %s rebuilt: %w", id, err)
	}
	if out == nil {
		out = []Rebuilt{}
	}
	return out, nil
}

// status is complete when every queued document has been rebuilt and no job
// on what the rebuild touched is still to run: the `distill` job of each
// document, the `assert` job that reads a re-distilled one again, and the
// `assert` job that repairs each stance citing one. A job on one of those
// targets enqueued later, for a reason of its own, holds the status at
// rebuilding until it has run too.
func status(ctx context.Context, tx pgx.Tx, docs []Rebuilt) (string, error) {
	ids := make([]string, len(docs))
	for i, d := range docs {
		if d.Outcome == OutcomePending {
			return StatusRebuilding, nil
		}
		ids[i] = d.ID
	}
	distilling, err := queue.Unfinished(ctx, tx, distiller.JobKind(), ids)
	if err != nil {
		return "", err
	}
	if len(distilling) > 0 {
		return StatusRebuilding, nil
	}
	stances, err := queryIDs(ctx, tx, `SELECT id FROM l2_stances WHERE evidence && $1::text[] ORDER BY id`, ids)
	if err != nil {
		return "", fmt.Errorf("reading the stances resting on rebuilt documents: %w", err)
	}
	targets := ids
	for _, st := range stances {
		targets = append(targets, l2.WithdrawalTarget+st)
	}
	asserting, err := queue.Unfinished(ctx, tx, l2.AssertKind(), targets)
	if err != nil {
		return "", err
	}
	if len(asserting) > 0 {
		return StatusRebuilding, nil
	}
	return StatusComplete, nil
}
