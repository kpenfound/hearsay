package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/hearsay/internal/telemetry"
)

type resyncCheck struct {
	read    bool
	pending []Exposure
}

func (r *Runtime) resync(ctx context.Context, h *hosted, rs Resyncer) {
	log := telemetry.Logger(ctx)
	interval := r.opts.Cadence.Interval(h.src)
	check := resyncCheck{read: r.opts.Exposure == nil}
	if check.read {
		log.WarnContext(ctx, "no exposure reader: a container that stopped being public while no delivery reached this process is not re-synced")
	}

	fails := 0
	for {
		progress, idle, err := r.resyncPass(ctx, h, rs, &check)
		if ctx.Err() != nil {
			log.InfoContext(ctx, "re-syncs stopped")
			return
		}
		var timer *time.Timer
		var wait <-chan time.Time
		switch {
		case err != nil:
			fails++
			h.resyncFails.Store(int64(fails))
			if progress {
				log.WarnContext(ctx, "re-sync failed for some containers, carrying on with the rest", "error", err, "failures", fails)
				continue
			}
			backoff := r.opts.Cadence.Backoff(interval, fails)
			log.WarnContext(ctx, "re-sync failed, retrying", "error", err, "failures", fails, "retry_in", backoff.String())
			timer = time.NewTimer(backoff)
			wait = timer.C
		case !idle:
			fails = 0
			h.resyncFails.Store(0)
			continue
		default:
			fails = 0
			h.resyncFails.Store(0)
		}
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "re-syncs stopped")
			return
		case <-h.gate.wake:
		case <-wait:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (r *Runtime) resyncPass(ctx context.Context, h *hosted, rs Resyncer, check *resyncCheck) (progress, idle bool, err error) {
	records, err := r.opts.Resyncs.Resyncs(ctx, h.src.ID)
	if err != nil {
		return false, false, fmt.Errorf("reading the re-syncs: %w", err)
	}
	var errs []error
	owed := false
	for _, rec := range records {
		if !rec.Owed || !h.gate.allow.Allows(h.src.ID, rec.Container) {
			continue
		}
		owed = true
		if err := r.resyncCall(ctx, h, rs, rec); err != nil {
			errs = append(errs, err)
			continue
		}
		progress = true
	}

	if !check.read {
		exposed, err := r.opts.Exposure.Exposed(ctx, h.src.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading which containers L0 serves as public: %w", err))
			return progress, false, errors.Join(errs...)
		}
		check.read = true
		check.pending = unsettled(h, exposed, records)
	}
	if !owed && len(check.pending) == 0 && len(errs) == 0 {
		return false, true, nil
	}

	log := telemetry.Logger(ctx)
	var still []Exposure
	for _, e := range check.pending {
		public, err := rs.Public(ctx, e.Container)
		if err == nil && !public {
			log.InfoContext(ctx, "container is no longer public and L0 serves it as public: a re-sync is owed", "container", e.Container)
			if err = r.opts.Resyncs.Owe(ctx, h.src.ID, e.Container); err == nil {
				progress = true
			}
		}
		if err != nil {
			still = append(still, e)
			errs = append(errs, fmt.Errorf("checking whether container %s is still public: %w", e.Container, err))
		}
	}
	check.pending = still
	return progress, false, errors.Join(errs...)
}

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
