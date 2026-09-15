package connector

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/hearsay/internal/telemetry"
)

// resyncCheck is the startup check for re-syncs nobody asked for: whether L0
// has been read for the containers it still serves as public, and which of
// those the connector has not yet been able to answer about.
type resyncCheck struct {
	read    bool
	pending []Exposure
}

// resync runs a [Resyncer]'s re-syncs for the life of the process: whatever the
// store says is owed, one call at a time with the position stored after each,
// then the startup check, then nothing until a delivery asks for another.
//
// Owed re-syncs go first. The check can fail on one container for as long as
// the source will not answer about it — a repository that was deleted — and a
// debt already recorded must not wait on that.
func (r *Runtime) resync(ctx context.Context, h *hosted, rs Resyncer) {
	log := telemetry.Logger(ctx)
	interval := r.opts.Cadence.Interval(h.src)
	check := resyncCheck{read: r.opts.Exposure == nil}
	if check.read {
		log.WarnContext(ctx, "no exposure reader: a container that stopped being public while no delivery reached this process is not re-synced")
	}

	fails := 0
	for {
		idle, err := r.resyncPass(ctx, h, rs, &check)
		if ctx.Err() != nil {
			log.InfoContext(ctx, "re-syncs stopped")
			return
		}
		if err != nil {
			fails++
			h.resyncFails.Store(int64(fails))
			wait := r.opts.Cadence.Backoff(interval, fails)
			log.WarnContext(ctx, "re-sync failed, retrying", "error", err, "failures", fails, "retry_in", wait.String())
			if !sleep(ctx, wait) {
				log.InfoContext(ctx, "re-syncs stopped")
				return
			}
			continue
		}
		fails = 0
		h.resyncFails.Store(0)
		if !idle {
			continue
		}
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "re-syncs stopped")
			return
		case <-h.gate.wake:
		}
	}
}

// resyncPass does one thing and reports whether there was nothing to do: one
// call of the first owed re-sync, or the startup check.
func (r *Runtime) resyncPass(ctx context.Context, h *hosted, rs Resyncer, check *resyncCheck) (bool, error) {
	records, err := r.opts.Resyncs.Resyncs(ctx, h.src.ID)
	if err != nil {
		return false, fmt.Errorf("reading the re-syncs: %w", err)
	}
	for _, rec := range records {
		// A container config stopped allowing owes nothing any more: the gate
		// would drop every event its walk emitted.
		if rec.Owed && h.gate.allow.Allows(h.src.ID, rec.Container) {
			return false, r.resyncCall(ctx, h, rs, rec)
		}
	}

	if !check.read {
		exposed, err := r.opts.Exposure.Exposed(ctx, h.src.ID)
		if err != nil {
			return false, fmt.Errorf("reading which containers L0 serves as public: %w", err)
		}
		check.read = true
		check.pending = unsettled(h, exposed, records)
	}
	if len(check.pending) == 0 {
		return true, nil
	}

	log := telemetry.Logger(ctx)
	var (
		still []Exposure
		errs  []error
	)
	for _, e := range check.pending {
		public, err := rs.Public(ctx, e.Container)
		if err == nil && !public {
			log.InfoContext(ctx, "container is no longer public and L0 serves it as public: a re-sync is owed", "container", e.Container)
			err = r.opts.Resyncs.Owe(ctx, h.src.ID, e.Container)
		}
		if err != nil {
			still = append(still, e)
			errs = append(errs, fmt.Errorf("checking whether container %s is still public: %w", e.Container, err))
		}
	}
	check.pending = still
	return false, errors.Join(errs...)
}

// unsettled is the exposed containers the check has to ask the connector
// about: those config still allows, and that hold a public artifact which
// arrived after the container's last finished re-sync. One that arrived before
// it is one that walk could not reach — a commit a force push removed — and
// would not reach again, so asking would re-walk the container on every start.
func unsettled(h *hosted, exposed []Exposure, records []Resync) []Exposure {
	byContainer := make(map[string]Resync, len(records))
	for _, rec := range records {
		byContainer[rec.Container] = rec
	}
	var out []Exposure
	for _, e := range exposed {
		if !h.gate.allow.Allows(h.src.ID, e.Container) {
			continue
		}
		if rec, ok := byContainer[e.Container]; ok && !rec.ResyncedAt.IsZero() && !e.LastPublic.After(rec.ResyncedAt) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// resyncCall makes one Resync call for an owed re-sync and records what it
// achieved before the next call is made: the position, or that the walk
// finished.
func (r *Runtime) resyncCall(ctx context.Context, h *hosted, rs Resyncer, rec Resync) error {
	log := telemetry.Logger(ctx)
	if rec.Cursor == "" {
		log.InfoContext(ctx, "re-syncing a container", "container", rec.Container)
	}
	res, err := rs.Resync(ctx, h.gate, rec.Container, rec.Cursor)
	if err != nil {
		return fmt.Errorf("re-syncing container %s: %w", rec.Container, err)
	}
	if !res.Done && res.Events == 0 && res.Next == rec.Cursor {
		return fmt.Errorf("re-syncing container %s made no progress: it emitted nothing and returned the cursor it was given", rec.Container)
	}
	if !res.Done {
		next := rec
		next.Cursor = res.Next
		if err := r.opts.Resyncs.Save(ctx, h.src.ID, next); err != nil {
			return fmt.Errorf("saving the re-sync position of container %s: %w", rec.Container, err)
		}
		return nil
	}
	finished, err := r.opts.Resyncs.Finish(ctx, h.src.ID, rec)
	if err != nil {
		return fmt.Errorf("recording the finished re-sync of container %s: %w", rec.Container, err)
	}
	if finished {
		log.InfoContext(ctx, "re-sync complete", "container", rec.Container)
	} else {
		log.InfoContext(ctx, "re-sync asked for again while it ran: starting it over", "container", rec.Container)
	}
	return nil
}
