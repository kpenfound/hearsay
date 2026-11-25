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
	// time. It bounds how long a new event waits to be enqueued.
	DefaultPumpInterval = 2 * time.Second
	// DefaultPumpBatch is how many events one read takes. A full batch means
	// there is more, and the pump goes round again without waiting.
	DefaultPumpBatch = 200
)

// PumpOptions are what a deployment or a test turns down. Every field may be
// left zero.
type PumpOptions struct {
	// Quiet delays a document until no new events arrive for this long.
	// Zero disables debouncing, including for zero-value PumpOptions.
	Quiet time.Duration
	// MaxDelay caps the debounce from the first enqueue. With Quiet > 0,
	// it must be at least Quiet.
	MaxDelay time.Duration
	// Interval is how long the pump waits after a read that found nothing.
	Interval time.Duration
	// Batch is how many events one read takes, held to what one read of the
	// feed returns.
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
// When Quiet > 0 the queue debounces each document: a burst waits for Quiet,
// capped by MaxDelay from its first enqueue, then costs one model call. The
// CLI settings are --distill-quiet and --distill-max-delay; zero Quiet keeps
// the ordinary pending-job deduplication.
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
	// Jobs counts distill job inserts and debounce schedule updates.
	Jobs int
	// Cursor is where the feed has been read to.
	Cursor l0.Cursor
}

// Once reads one batch of the feed and enqueues what it implies.
//
// The jobs and the new cursor are one transaction: either a batch's jobs exist
// and the cursor has moved past them, or neither happened and the batch is read
// again. Enqueueing is deduplicated on (kind, target).
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

	seen := map[string]bool{}
	events := l0.New(tx)
	for _, change := range changes {
		target, ok, err := TargetOf(ctx, change.Event, events)
		if err != nil {
			return Progress{}, err
		}
		head, headed, err := threadHeadOf(ctx, change.Event, events)
		if err != nil {
			return Progress{}, err
		}
		for _, target := range []struct {
			id string
			ok bool
		}{{target, ok}, {head, headed}} {
			if !target.ok || seen[target.id] {
				continue
			}
			seen[target.id] = true

			req := queue.Request{Kind: JobKind(), TargetID: target.id}
			if p.opts.Quiet > 0 {
				req.Debounce = &queue.Debounce{Quiet: p.opts.Quiet, Max: p.opts.MaxDelay}
			}
			enqueued, err := queue.Enqueue(ctx, tx, req)
			if err != nil {
				return Progress{}, err
			}
			if enqueued.Stored {
				progress.Jobs++
			}
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
// off.
//
// A tombstone re-derives the document the retracted artifact belonged to: the
// conversation it was part of, or its own document if it made one. A tombstone
// that names the conversation (`thread`, else `parent`) is taken at its word.
// One that names only its `target` is resolved by reading the retracted
// artifact from behind the tombstone and asking the same question of it. A
// tombstone for an artifact L0 holds nothing of belongs to no document.
func TargetOf(ctx context.Context, ev connector.Event, hidden Hidden) (string, bool, error) {
	if ev.Kind != connector.KindTombstone && ev.Payload.BaseKind != connector.KindTombstone {
		target, ok := targetOf(ev)
		return target, ok, nil
	}
	if root := conversationOf(ev); root != "" && ev.Payload.Container.Kind != connector.ContainerChannel {
		return l1.DocID(ev.Source, root), true, nil
	}

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

func threadHeadOf(ctx context.Context, ev connector.Event, events *l0.Store) (string, bool, error) {
	if ev.Kind == connector.KindTombstone || ev.Payload.BaseKind == connector.KindTombstone {
		if ev.Payload.Container.Kind != connector.ContainerChannel {
			return "", false, nil
		}
		retracted, err := events.Retracted(ctx, ev.Source, ev.Payload.Target)
		if errors.Is(err, l0.ErrNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("reading what %s retracts: %w", ev.NativeID, err)
		}
		ev = retracted
	}
	if !distilled(ev) || !l1.ChatRoot(ev) {
		return "", false, nil
	}
	replies, err := events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: ev.Source, Thread: ev.Payload.Artifact}, Limit: 1})
	if err != nil {
		return "", false, fmt.Errorf("reading what hangs off %s: %w", ev.Payload.Artifact, err)
	}
	if len(replies) == 0 {
		return "", false, nil
	}
	return l1.DocID(ev.Source, ev.Payload.Artifact), true, nil
}

var undistilled = []connector.Kind{connector.KindAgentSession, connector.KindAgentTurn, connector.KindToolCall, connector.KindNextAction, connector.KindCommand}

func distilled(ev connector.Event) bool {
	return !slices.Contains(undistilled, ev.Kind) && !slices.Contains(undistilled, ev.Payload.BaseKind)
}

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

func conversationOf(ev connector.Event) string {
	if ev.Payload.Thread != "" {
		return ev.Payload.Thread
	}
	return ev.Payload.Parent
}
