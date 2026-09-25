package assertworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

const gestureConsumer = "assert-worker:gestures"

// GestureFollower places chat reaction changes on the assertion queue. Its
// cursor and the jobs commit together, so a restart cannot miss a reaction. A
// read-only source's reactions are not gestures, and are passed over.
type GestureFollower struct {
	pool       *pgxpool.Pool
	repo       config.Repo
	connectors *connector.Registry
}

// NewGestureFollower builds the reaction feed reader.
func NewGestureFollower(pool *pgxpool.Pool, repo config.Repo, connectors *connector.Registry) *GestureFollower {
	return &GestureFollower{pool: pool, repo: repo, connectors: connectors}
}

// Run follows the feed until the context is cancelled.
func (f *GestureFollower) Run(ctx context.Context) error {
	tick := time.NewTicker(DefaultFollowInterval)
	defer tick.Stop()
	for {
		for ctx.Err() == nil {
			n, err := f.Once(ctx)
			if err != nil {
				telemetry.Logger(ctx).ErrorContext(ctx, "following gestures failed", "error", err)
				break
			}
			if n < DefaultFollowBatch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Once enqueues one batch of changes and saves its cursor atomically.
func (f *GestureFollower) Once(ctx context.Context) (int, error) {
	from, err := l0.NewCursors(f.pool).Load(ctx, gestureConsumer)
	if err != nil {
		return 0, err
	}
	changes, err := l0.New(f.pool).Changes(ctx, from, l0.Filter{}, DefaultFollowBatch)
	if err != nil || len(changes) == 0 {
		return 0, err
	}
	return len(changes), pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		for _, change := range changes {
			ev := change.Event
			src, ok := f.repo.Source(ev.Source)
			if !ok || (ev.Kind != connector.KindReaction && ev.Kind != connector.KindTombstone) {
				continue
			}
			_, eligible, err := f.connectors.ReactionGestures(src)
			if err != nil {
				return err
			}
			if !eligible {
				continue
			}
			if ev.Kind == connector.KindTombstone && !strings.Contains(ev.Payload.Target, ":reaction:") {
				continue
			}
			_, err = queue.Enqueue(ctx, tx, queue.Request{Kind: l2.AssertKind(), TargetID: l2.GestureTarget + ev.ID,
				SerialKey: l2.ScopeKey(f.repo, ev.Source, ev.Payload.Container.NativeID)})
			if err != nil {
				return err
			}
		}
		_, err := l0.NewCursors(tx).Save(ctx, gestureConsumer, changes[len(changes)-1].Cursor)
		return err
	})
}

func (a *Asserter) applyReactionGesture(ctx context.Context, job queue.Job) error {
	id := strings.TrimPrefix(job.TargetID, l2.GestureTarget)
	ev, err := a.events.Get(ctx, id)
	if errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
		return nil
	}
	if err != nil {
		return err
	}
	src, ok := a.repo.Source(ev.Source)
	if !ok {
		return nil
	}
	gestures, eligible, err := a.connectors.ReactionGestures(src)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	var req l2.GestureRequest
	var actorID string
	switch ev.Kind {
	case connector.KindTombstone:
		if !strings.Contains(ev.Payload.Target, ":reaction:") {
			return nil
		}
		g, err := l2.New(a.pool).GestureByEvent(ctx, connector.EventID(ev.Source, ev.Payload.Target))
		if errors.Is(err, l2.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		req = l2.GestureRequest{Event: ev.ID, Principal: g.Principal, Action: l2.GestureUndo, Undoes: g.Event}
	case connector.KindReaction:
		var native struct {
			Emoji string `json:"emoji"`
		}
		if err := json.Unmarshal(ev.Payload.Native, &native); err != nil {
			return fmt.Errorf("reading reaction %s: %w", ev.ID, err)
		}
		switch native.Emoji {
		case gestures.Ratify:
			req.Action = l2.GestureRatify
		case gestures.Demote:
			req.Action = l2.GestureDemote
		default:
			return nil
		}
		userID := ""
		if ev.Payload.Author != nil {
			userID = ev.Payload.Author.NativeID
		}
		actorID = userID
		resolver, err := a.repo.Resolver()
		if err != nil {
			return err
		}
		resolved := resolver.Resolve(connector.Identity{Source: ev.Source, Kind: connector.IdentityUser, NativeID: userID})
		if resolved.Status != principal.Resolved || userID == "" {
			telemetry.Logger(ctx).DebugContext(ctx, "reaction identity not mapped", "source", ev.Source, "user_id", userID)
			return nil
		}
		req.Event, req.Principal = ev.ID, resolved.Principal.ID
		message, found, err := a.events.CurrentArtifact(ctx, ev.Source, ev.Payload.Parent)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("reaction %s awaits its message", ev.ID)
		}
		holders, err := l1.New(a.pool).Holders(ctx, []string{message.ID})
		if err != nil {
			return err
		}
		for _, h := range holders[message.ID] {
			req.Documents = append(req.Documents, h.ID)
		}
		if len(req.Documents) == 0 {
			return fmt.Errorf("reaction %s awaits its containing L1 document", ev.ID)
		}
	default:
		return nil
	}
	return pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		_, _, err := l2.New(tx).ApplyGesture(ctx, a.repo, job.SerialKey, req)
		if errors.Is(err, l2.ErrNotFound) && req.Action != l2.GestureUndo {
			pending, pendingErr := queue.Unfinished(ctx, tx, l2.AssertKind(), req.Documents)
			if pendingErr != nil {
				return pendingErr
			}
			for _, doc := range req.Documents {
				if pending[doc] {
					return fmt.Errorf("reaction %s awaits assertion of %s: %w", ev.ID, doc, err)
				}
			}
		}
		if errors.Is(err, l2.ErrNotAllowed) || errors.Is(err, l2.ErrNotFound) {
			telemetry.Logger(ctx).DebugContext(ctx, "reaction gesture refused", "source", ev.Source, "user_id", actorID, "l0_id", ev.ID)
			return nil
		}
		return err
	})
}
