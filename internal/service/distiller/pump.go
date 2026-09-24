package distiller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Consumer is the name this service reads the change feed under. One name for
// every replica: they share a position, and the cursor only moves forward.
const Consumer = "distiller"

// The pump's defaults.
const (
	// DefaultPumpInterval is how often the feed is read when it was empty last
	// time. L0 has no notification of its own — a connector's write is one
	// statement, and adding one would be a second mechanism to keep in step
	// with the cursor — so this is what bounds how late a distillation can be.
	DefaultPumpInterval = 2 * time.Second
	// DefaultPumpBatch is how many events one read takes. A full batch means
	// there is more, and the pump goes round again without waiting.
	DefaultPumpBatch = 200
)

// PumpOptions are what a deployment or a test turns down. Every field may be
// left zero.
type PumpOptions struct {
	// Interval is how long the pump waits after a read that found nothing.
	Interval time.Duration
	// Batch is how many events one read takes. It is held to what one read of
	// the feed actually returns, because the drain loop reads a full batch as
	// "there is more": a batch above the store's own cap could never be
	// reached, and the pump would wait an interval between batches for the
	// whole of a backfill.
	Batch int
}

func (o PumpOptions) withDefaults() PumpOptions {
	if o.Interval <= 0 {
		o.Interval = DefaultPumpInterval
	}
	if o.Batch <= 0 {
		o.Batch = DefaultPumpBatch
	}
	o.Batch = l0.Limit(o.Batch)
	return o
}

// Pump is the half of the distiller that decides what to distil: it reads the
// L0 change feed and enqueues a `distill` job for the document each event
// belongs to.
//
// It is deliberately not the half that distils. An event is small and arrives
// in bursts — a pull request opened, then five reviews in a minute — and the
// queue collapses a burst into one pending job per document, so the model is
// called once per document rather than once per event (ADR-0007).
type Pump struct {
	pool    *pgxpool.Pool
	events  *l0.Store
	cursors *l0.Cursors
	opts    PumpOptions
}

// NewPump builds the feed reader. The caller owns the pool.
func NewPump(pool *pgxpool.Pool, opts PumpOptions) *Pump {
	return &Pump{
		pool:    pool,
		events:  l0.New(pool),
		cursors: l0.NewCursors(pool),
		opts:    opts.withDefaults(),
	}
}

// Batch is how many events one read of the feed takes, after the defaults and
// the store's own cap have been applied. It is exported because it is what the
// drain loop compares a read against, and a caller that asked for more than one
// read returns should be able to see what it got.
func (p *Pump) Batch() int { return p.opts.Batch }

// Run reads the feed until ctx is cancelled, and returns nil when it stops that
// way. A read that fails is logged and retried at the next tick: the feed is a
// cursor over a table, so nothing is lost by being late.
func (p *Pump) Run(ctx context.Context) error {
	log := telemetry.Logger(ctx)
	log.InfoContext(ctx, "reading the l0 change feed",
		"consumer", Consumer, "interval", p.opts.Interval.String(), "batch", p.opts.Batch)

	tick := time.NewTicker(p.opts.Interval)
	defer tick.Stop()
	for {
		// Drain: a full batch means there is more of the feed to read, and
		// waiting for the next tick between batches would make a backfill take
		// one interval per batch.
		for ctx.Err() == nil {
			progress, err := p.Once(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.ErrorContext(ctx, "reading the l0 change feed failed", "error", err)
				}
				break
			}
			if progress.Events < p.opts.Batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "stopped reading the l0 change feed")
			return nil
		case <-tick.C:
		}
	}
}

// Progress is what one read of the feed did.
type Progress struct {
	// Events is how many events the read returned. A number equal to the batch
	// size means there is more.
	Events int
	// Jobs is how many distill jobs were written. It is lower than Events
	// whenever a burst collapsed onto one document, which is the point.
	Jobs int
	// Cursor is where the feed has been read to.
	Cursor l0.Cursor
}

// Once reads one batch of the feed and enqueues what it implies.
//
// The jobs and the new cursor are one transaction. That is what makes the pump
// safe to restart: either a batch's jobs exist and the cursor has moved past
// them, or neither happened and the batch is read again. Enqueueing is
// deduplicated on (kind, target), so reading a batch twice costs one statement
// rather than one distillation.
func (p *Pump) Once(ctx context.Context) (Progress, error) {
	from, err := p.cursors.Load(ctx, Consumer)
	if err != nil {
		return Progress{}, err
	}
	changes, err := p.events.Changes(ctx, from, l0.Filter{}, p.opts.Batch)
	if err != nil {
		return Progress{}, err
	}
	if len(changes) == 0 {
		return Progress{Cursor: from}, nil
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Progress{}, fmt.Errorf("enqueueing distillations: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	progress := Progress{Events: len(changes), Cursor: changes[len(changes)-1].Cursor}
	// One job per document per batch. The queue collapses duplicates against
	// what is already pending; this collapses them against this batch, which is
	// where a burst of reviews on one pull request actually arrives.
	seen := map[string]bool{}
	events := l0.New(tx)
	for _, change := range changes {
		target, ok, err := TargetOf(ctx, change.Event, events)
		if err != nil {
			return Progress{}, err
		}
		if !ok || seen[target] {
			continue
		}
		seen[target] = true
		// No trace carrier: internal/telemetry has no propagator yet
		// (ADR-0008's tracing is a later issue), and a carrier this package
		// invented would be one the worker could not read.
		enqueued, err := queue.Enqueue(ctx, tx, queue.Request{Kind: JobKind(), TargetID: target})
		if err != nil {
			return Progress{}, err
		}
		if enqueued.Stored {
			progress.Jobs++
		}
	}
	if _, err := l0.NewCursors(tx).Save(ctx, Consumer, progress.Cursor); err != nil {
		return Progress{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Progress{}, fmt.Errorf("enqueueing distillations: %w", err)
	}
	return progress, nil
}

// Hidden is the read [TargetOf] makes past a tombstone: the current revision of
// an artifact a tombstone covers, and [l0.ErrNotFound] where there is none.
// [*l0.Store] implements it.
type Hidden interface {
	Retracted(ctx context.Context, source, artifact string) (connector.Event, error)
}

// TargetOf is the document an event belongs to, and false for an event that
// belongs to none: an agent session event (see distilled) belongs to none.
//
// An artifact that makes a document of its own is its own target. Everything
// else — a comment, a review, a reply — is part of the conversation it hangs
// off, which is what makes a pull request with its reviews one document rather
// than five.
//
// A tombstone re-derives the document the retracted artifact belonged to: the
// conversation it was part of, or its own document if it made one. A tombstone
// that names the conversation (`thread`, else `parent`) is taken at its word.
// One that names only its `target` — which is all docs/connector-contract.md
// requires, and what the GitHub connector sends for a deleted comment — is
// resolved by reading the retracted artifact from behind the tombstone and
// asking the same question of it. Without that read, a deleted comment's job
// would go to the comment's own document, which never existed, and the issue
// that quotes the comment would keep it. A tombstone for an artifact L0 holds
// nothing of belongs to no document.
//
// Distillation then deletes or re-reads L1, and deletion enqueues the affected
// L2 stances on their serialized scope keys.
func TargetOf(ctx context.Context, ev connector.Event, hidden Hidden) (string, bool, error) {
	if ev.Kind != connector.KindTombstone && ev.Payload.BaseKind != connector.KindTombstone {
		target, ok := targetOf(ev)
		return target, ok, nil
	}
	if root := conversationOf(ev); root != "" && ev.Payload.Container.Kind != connector.ContainerChannel {
		return l1.DocID(ev.Source, root), true, nil
	}
	// The target is there: Event.Validate refuses a tombstone without one before
	// it reaches L0.
	retracted, err := hidden.Retracted(ctx, ev.Source, ev.Payload.Target)
	if errors.Is(err, l0.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading what %s retracts: %w", ev.NativeID, err)
	}
	target, ok := targetOf(retracted)
	return target, ok, nil
}

// undistilled are the kinds that belong to no document. An agent's session,
// its turns, tool calls and next actions are provenance — what an agent was doing when
// it said something — rather than team knowledge, and distilling every tool
// call would flood L1 (#22). A command a person gave Hearsay is control
// traffic, not team content (ADR-0022).
var undistilled = []connector.Kind{connector.KindAgentSession, connector.KindAgentTurn, connector.KindToolCall, connector.KindNextAction, connector.KindCommand}

// distilled reports whether an event's kind, or the core kind an extension
// kind behaves like, is one the distiller makes documents from. Whatever the
// event says about the conversation it is part of, an agent session event is
// not.
func distilled(ev connector.Event) bool {
	return !slices.Contains(undistilled, ev.Kind) && !slices.Contains(undistilled, ev.Payload.BaseKind)
}

// targetOf is the document an event that is not a tombstone belongs to.
func targetOf(ev connector.Event) (string, bool) {
	if !distilled(ev) {
		return "", false
	}
	if ev.Kind == connector.KindDocument || ev.Payload.BaseKind == connector.KindDocument || ev.Kind == connector.KindTranscript || ev.Payload.BaseKind == connector.KindTranscript {
		return l1.DocID(ev.Source, ev.Payload.Artifact), true
	}
	if _, ok := l1.KindFor(ev); ok {
		return l1.DocID(ev.Source, ev.Payload.Artifact), true
	}
	if ev.Payload.Container.Kind == connector.ContainerChannel && (ev.Kind == connector.KindMessage || ev.Payload.BaseKind == connector.KindMessage) {
		if ev.Payload.Thread != "" {
			return l1.DocID(ev.Source, ev.Payload.Thread), true
		}
		return l1.DocID(ev.Source, l1.ChatWindowKey(ev)), true
	}
	if root := conversationOf(ev); root != "" {
		return l1.DocID(ev.Source, root), true
	}
	return "", false
}

// conversationOf is the artifact an event hangs off: its thread, or the thing
// it is a reply to on a source that has no threads
// (docs/connector-contract.md).
func conversationOf(ev connector.Event) string {
	if ev.Payload.Thread != "" {
		return ev.Payload.Thread
	}
	return ev.Payload.Parent
}
