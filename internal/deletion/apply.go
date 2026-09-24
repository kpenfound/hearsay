package deletion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// Applied is what [Apply] did.
type Applied struct {
	// Deletion is the id of the deletion record.
	Deletion string `json:"deletion"`
	// Operator is the configured human principal who applied it.
	Operator string `json:"operator"`
	// Retraction is the `deletion` event written under source `hearsay`.
	Retraction string `json:"retraction"`
	// Events are the L0 events redacted. An event the selector matched that
	// an earlier deletion had already redacted is in Preview.Events and not
	// here.
	Events []string `json:"events"`
	// Documents are the L1 documents queued for re-distillation.
	Documents []string `json:"documents"`
	// Preview is the forward walk the deletion was applied from.
	Preview Preview `json:"preview"`
}

// CheckOperator refuses an operator who is not a configured human principal.
// The CLI calls it before it opens the database, and [Apply] again before it
// writes anything.
func CheckOperator(repo config.Repo, id string) error {
	if id == "" {
		return fmt.Errorf("applying a deletion needs --principal")
	}
	p, ok := repo.Principal(id)
	if !ok {
		return fmt.Errorf("unknown principal %q: the operator must be a configured human", id)
	}
	if p.Kind != principal.KindHuman {
		return fmt.Errorf("principal %q is a %s: the operator must be a configured human", id, p.Kind)
	}
	return nil
}

// Apply deletes what a selector covers, as operator. In one transaction it
// walks provenance forward the way [Walk] does, records the deletion, redacts
// the covered L0 events, writes Hearsay's own `deletion` event, and enqueues a
// `distill` job for every L1 document the walk found. Anything that fails
// rolls all of it back.
//
// The running distiller does the rebuild: each document is re-distilled from
// what is still visible, or deleted if nothing is. L2 follows the documents
// (issue #159).
func Apply(ctx context.Context, pool *pgxpool.Pool, repo config.Repo, sel Selector, reason, operator string) (Applied, error) {
	if err := check(sel, reason); err != nil {
		return Applied{}, err
	}
	if err := CheckOperator(repo, operator); err != nil {
		return Applied{}, err
	}
	id, err := newID()
	if err != nil {
		return Applied{}, err
	}
	selector, err := json.Marshal(sel)
	if err != nil {
		return Applied{}, fmt.Errorf("encoding the selector: %w", err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Applied{}, fmt.Errorf("starting deletion %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	p, err := walk(ctx, tx, repo, sel, reason)
	if err != nil {
		return Applied{}, err
	}
	deleted, err := l0.Delete(ctx, tx, l0.Deletion{
		ID: id, Operator: operator, Reason: reason, Selector: selector,
		Events: p.Events, Documents: p.Documents, Time: time.Now().UTC(),
	})
	if err != nil {
		return Applied{}, err
	}
	for _, doc := range p.Documents {
		if _, err := queue.Enqueue(ctx, tx, queue.Request{Kind: distiller.JobKind(), TargetID: doc}); err != nil {
			return Applied{}, fmt.Errorf("enqueueing the re-distillation of %s: %w", doc, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Applied{}, fmt.Errorf("committing deletion %s: %w", id, err)
	}
	return Applied{
		Deletion: id, Operator: operator, Retraction: deleted.Retraction,
		Events: deleted.Events, Documents: p.Documents, Preview: p,
	}, nil
}

// newID mints a deletion id: `del_` and 128 random bits in hex.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("minting a deletion id: %w", err)
	}
	return "del_" + hex.EncodeToString(b[:]), nil
}
