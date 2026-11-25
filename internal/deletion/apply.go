package deletion

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
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
	// Preview is the forward walk the deletion was applied from. For a replay
	// it holds only the recorded selector.
	Preview Preview `json:"preview"`
	// Replay is set when the deletion was already recorded under the request's
	// id: this call wrote nothing and Events are the ones that deletion
	// redacted.
	Replay bool `json:"replay,omitempty"`
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

// Apply deletes what a selector covers, as operator. It is [ApplyRequest] for
// one selector and a random deletion id.
func Apply(ctx context.Context, pool *pgxpool.Pool, repo config.Repo, sel Selector, reason, operator string) (Applied, error) {
	return ApplyRequest(ctx, pool, repo, Request{Selectors: []Selector{sel}, Reason: reason, Operator: operator})
}

// Steps of a deletion, which [Request.After] is called with.
const (
	// StepWalked is after the walk, before anything is written.
	StepWalked = "walked"
	// StepRecorded is after the deletion record, its event and the redaction
	// are written.
	StepRecorded = "recorded"
	// StepEnqueued is after each document's re-distillation is queued.
	StepEnqueued = "enqueued"
)

// Request is one deletion, applied as a whole.
type Request struct {
	// ID is the deletion's id. Empty mints a random one. A request whose id a
	// deletion already holds is answered with that deletion and writes nothing
	// ([Applied.Replay]), however many callers race to it.
	ID string
	// Selectors are what to delete: the union of what each names.
	Selectors []Selector
	// Recorded is the selector stored on the deletion. Zero stores the one
	// selector there is.
	Recorded *Selector
	// Reason is why, and may not be empty.
	Reason string
	// Operator is the configured human applying it.
	Operator string
	// Guard is called inside the transaction with what the selectors walked
	// to, before anything is written. An error stops the deletion and is
	// returned as it is; nothing has been written.
	Guard func(ctx context.Context, tx pgx.Tx, p Preview) error
	// After is called inside the transaction after each [StepWalked],
	// [StepRecorded] and [StepEnqueued]. An error rolls the whole deletion
	// back, and is how a test fails it part-way.
	After func(step string) error
}

func (r Request) step(name string) error {
	if r.After == nil {
		return nil
	}
	return r.After(name)
}

// ApplyRequest deletes what the selectors cover, as one operator's one
// deletion. In one transaction it walks provenance forward the way [Walk]
// does, records the deletion, redacts the covered L0 events, writes Hearsay's
// own `deletion` event, takes out the pins the gestures from those events
// made ([l2.Store.ResyncDeletedPins]), and enqueues a `distill` job for every
// L1 document the walk found. Anything that fails rolls all of it back. The
// gestures themselves stay in the ledger; a deleted event takes its gesture out
// of force wherever it is read.
//
// The artifacts of a conversation source it covers are held while it runs, so
// that a message posted around the deletion either is covered by it or finds
// the message deleted ([l0.Store.SerializeArtifact]).
//
// The running distiller does the rebuild: each document is re-distilled from
// what is still visible, or deleted if nothing is, and L2 follows the
// documents as it follows a source tombstone.
func ApplyRequest(ctx context.Context, pool *pgxpool.Pool, repo config.Repo, req Request) (Applied, error) {
	if len(req.Selectors) == 0 {
		return Applied{}, fmt.Errorf("a deletion needs a selector")
	}
	for _, sel := range req.Selectors {
		if err := check(sel, req.Reason); err != nil {
			return Applied{}, err
		}
	}
	if err := CheckOperator(repo, req.Operator); err != nil {
		return Applied{}, err
	}
	id := req.ID
	if id == "" {
		var err error
		if id, err = newID(); err != nil {
			return Applied{}, err
		}
	}
	recorded := req.Selectors[0]
	if req.Recorded != nil {
		recorded = *req.Recorded
	}
	selector, err := json.Marshal(recorded)
	if err != nil {
		return Applied{}, fmt.Errorf("encoding the selector: %w", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return Applied{}, fmt.Errorf("starting deletion %s: %w", id, err)
	}
	defer conn.Release()
	held, err := conversationArtifacts(ctx, conn, repo, req.Selectors)
	if err != nil {
		return Applied{}, fmt.Errorf("starting deletion %s: %w", id, err)
	}
	unlock, err := l0.HoldArtifacts(ctx, conn, held)
	if err != nil {
		return Applied{}, fmt.Errorf("starting deletion %s: %w", id, err)
	}
	defer unlock()
	unhold, err := l2.HoldPins(ctx, conn)
	if err != nil {
		return Applied{}, fmt.Errorf("starting deletion %s: %w", id, err)
	}
	defer unhold()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Applied{}, fmt.Errorf("starting deletion %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `LOCK TABLE judgment_input_guard IN ROW EXCLUSIVE MODE`); err != nil {
		return Applied{}, fmt.Errorf("holding judgment evidence for deletion: %w", err)
	}

	if req.ID != "" {
		prior, found, err := priorDeletion(ctx, tx, id)
		if err != nil || found {
			return prior, err
		}
	}
	p, err := walk(ctx, tx, repo, req.Selectors, req.Reason)
	if err != nil {
		return Applied{}, err
	}
	if req.Guard != nil {
		if err := req.Guard(ctx, tx, p); err != nil {
			return Applied{}, err
		}
	}
	if err := req.step(StepWalked); err != nil {
		return Applied{}, err
	}
	deleted, err := l0.Delete(ctx, tx, l0.Deletion{
		ID: id, Operator: req.Operator, Reason: req.Reason, Selector: selector,
		Events: p.Events, Documents: p.Documents, Time: time.Now().UTC(),
	})
	if err != nil {
		return Applied{}, err
	}
	if err := l2.New(tx).ResyncDeletedPins(ctx, p.Events); err != nil {
		return Applied{}, err
	}
	if err := req.step(StepRecorded); err != nil {
		return Applied{}, err
	}
	for _, doc := range p.Documents {
		if _, err := queue.Enqueue(ctx, tx, queue.Request{Kind: distiller.JobKind(), TargetID: doc}); err != nil {
			return Applied{}, fmt.Errorf("enqueueing the re-distillation of %s: %w", doc, err)
		}
		if _, err := queue.Expedite(ctx, tx, distiller.JobKind(), doc); err != nil {
			return Applied{}, fmt.Errorf("expediting the re-distillation of %s: %w", doc, err)
		}
		if err := req.step(StepEnqueued); err != nil {
			return Applied{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Applied{}, fmt.Errorf("committing deletion %s: %w", id, err)
	}
	return Applied{
		Deletion: id, Operator: req.Operator, Retraction: deleted.Retraction,
		Events: deleted.Events, Documents: p.Documents, Preview: p,
	}, nil
}

func priorDeletion(ctx context.Context, tx pgx.Tx, id string) (Applied, bool, error) {
	var (
		a        = Applied{Deletion: id, Replay: true}
		selector []byte
	)
	err := tx.QueryRow(ctx, `SELECT operator, retraction, events, documents, selector FROM l0_deletions WHERE id = $1`, id).
		Scan(&a.Operator, &a.Retraction, &a.Events, &a.Documents, &selector)
	if errors.Is(err, pgx.ErrNoRows) {
		return Applied{}, false, nil
	}
	if err != nil {
		return Applied{}, false, fmt.Errorf("reading deletion %s: %w", id, err)
	}
	if err := json.Unmarshal(selector, &a.Preview.Selector); err != nil {
		return Applied{}, false, fmt.Errorf("decoding the selector of deletion %s: %w", id, err)
	}
	return a, true, nil
}

func conversationArtifacts(ctx context.Context, conn *pgxpool.Conn, repo config.Repo, sels []Selector) ([]l0.ArtifactRef, error) {
	held := map[string]bool{}
	for _, src := range repo.Sources {
		if src.Type == conversation.Type {
			held[src.ID] = true
		}
	}
	if len(held) == 0 {
		return nil, nil
	}
	var ids []string
	for _, sel := range sels {
		found, err := selectEvents(ctx, conn, repo, sel)
		if err != nil {
			return nil, err
		}
		ids = append(ids, found...)
	}
	rows, err := conn.Query(ctx, `SELECT DISTINCT source, artifact FROM l0_events WHERE id = ANY($1)`, unique(ids))
	if err != nil {
		return nil, err
	}
	refs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (l0.ArtifactRef, error) {
		var ref l0.ArtifactRef
		return ref, r.Scan(&ref.Source, &ref.Artifact)
	})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(refs, func(r l0.ArtifactRef) bool { return !held[r.Source] }), nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("minting a deletion id: %w", err)
	}
	return "del_" + hex.EncodeToString(b[:]), nil
}
