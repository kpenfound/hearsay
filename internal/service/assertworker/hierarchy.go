package assertworker

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// HierarchyConsumer is the name the worker reads the L0 change feed under for
// the tracker hierarchy. One name for every replica, as the distiller's.
const HierarchyConsumer = "assert-worker:hierarchy"

// The follower's defaults: the distiller's pump's, for the same reasons.
const (
	DefaultFollowInterval = 2 * time.Second
	DefaultFollowBatch    = 200
)

// Placements is where the trackers configured scopes map put their items now,
// as L0 holds them: every issue whose current revision is `part_of` another,
// in every tracker a scope maps ([l2.PlacementOf]). It is what seeding merges
// as the tracker's hierarchy.
func Placements(ctx context.Context, pool *pgxpool.Pool, repo config.Repo) ([]l2.Placement, error) {
	events := l0.New(pool)
	var trackers []config.SourceRef
	for _, s := range repo.Scopes {
		if s.Tracker.Source != "" && !slices.Contains(trackers, s.Tracker) {
			trackers = append(trackers, s.Tracker)
		}
	}
	var out []l2.Placement
	for _, t := range trackers {
		placed, err := events.Placed(ctx, t.Source, t.Project)
		if err != nil {
			return nil, err
		}
		for _, ev := range placed {
			if p, ok := l2.PlacementOf(repo, ev); ok {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// Follower keeps the tracker hierarchy in L2 in step with L0 between two
// startups: it reads the change feed, and for every tracker item an event
// changed it places the item where its current revision says ([l2.Store.Place]).
// Seeding at startup is the whole hierarchy; this is each change after it, and
// it makes no model call.
type Follower struct {
	pool     *pgxpool.Pool
	repo     config.Repo
	interval time.Duration
	batch    int
}

// NewFollower builds the feed reader. A zero interval or batch is the default.
func NewFollower(pool *pgxpool.Pool, repo config.Repo, interval time.Duration, batch int) *Follower {
	if interval <= 0 {
		interval = DefaultFollowInterval
	}
	if batch <= 0 {
		batch = DefaultFollowBatch
	}
	return &Follower{pool: pool, repo: repo, interval: interval, batch: l0.Limit(batch)}
}

// Run reads the feed until ctx is cancelled, and returns nil when it stops that
// way. A read that fails is logged and retried at the next tick.
func (f *Follower) Run(ctx context.Context) error {
	log := telemetry.Logger(ctx)
	tick := time.NewTicker(f.interval)
	defer tick.Stop()
	for {
		for ctx.Err() == nil {
			n, err := f.Once(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.ErrorContext(ctx, "following the tracker hierarchy failed", "error", err)
				}
				break
			}
			if n < f.batch {
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

// Once reads one batch of the feed, places what it changed, and returns how
// many events it read. The placements and the new cursor are one transaction,
// so a batch is placed once or read again.
//
// An item is placed from its current revision, not from the event that
// changed it: a revision that arrives late, or a tombstone, says nothing about
// where the item is now. An item with no current revision is placed nowhere.
func (f *Follower) Once(ctx context.Context) (int, error) {
	from, err := l0.NewCursors(f.pool).Load(ctx, HierarchyConsumer)
	if err != nil {
		return 0, err
	}
	changes, err := l0.New(f.pool).Changes(ctx, from, l0.Filter{}, f.batch)
	if err != nil {
		return 0, err
	}
	if len(changes) == 0 {
		return 0, nil
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("following the tracker hierarchy: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	events, graph := l0.New(tx), l2.New(tx)
	configured := l2.Configured(f.repo)
	seen := map[[2]string]bool{}
	for _, change := range changes {
		ev := change.Event
		artifact := ev.Payload.Artifact
		switch {
		case ev.Kind == connector.KindTombstone || ev.Payload.BaseKind == connector.KindTombstone:
			artifact = ev.Payload.Target
		case ev.Kind != connector.KindIssue && ev.Payload.BaseKind != connector.KindIssue:
			continue
		}
		item, ok := l2.TrackerItem(f.repo, ev.Source, artifact)
		if !ok || seen[[2]string{ev.Source, artifact}] {
			continue
		}
		seen[[2]string{ev.Source, artifact}] = true
		placement := l2.Placement{Item: item}
		current, found, err := events.CurrentArtifact(ctx, ev.Source, artifact)
		if err != nil {
			return 0, err
		}
		if found {
			if p, ok := l2.PlacementOf(f.repo, current); ok {
				placement = p
			}
		}
		if err := graph.Place(ctx, configured, placement); err != nil {
			return 0, err
		}
	}
	if _, err := l0.NewCursors(tx).Save(ctx, HierarchyConsumer, changes[len(changes)-1].Cursor); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("following the tracker hierarchy: %w", err)
	}
	return len(changes), nil
}
