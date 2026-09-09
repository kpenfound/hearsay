package queue

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Handler runs one job. It is the part the queue does not own: what a
// `distill` or an `assert` job does belongs to the service that owns the kind.
//
// Returning nil marks the job done. Returning an error schedules a retry, or
// fails the job for good once its attempts are spent; the error's message is
// stored on the row and logged, so it says what went wrong and carries no
// document text, no prompt and no completion (ADR-0008).
//
// A handler must be safe to run twice on the same job. Delivery is
// at-least-once: a worker that dies mid-job leaves a job that is reclaimed and
// run again, and that is the semantics ADR-0007 chose knowing both of
// Hearsay's workers are idempotent.
//
// The context is cancelled when the worker is shutting down and when the
// job's lease has been lost to another worker, which is the signal to stop
// working on it: another worker is already running it.
type Handler func(ctx context.Context, job Job) error

// Worker is the loop over a [Client]: claim, run, report, repeat. It wakes on
// a notification for its kind and, failing that, on the polling floor, so a
// missed notification delays a job rather than stranding it (ADR-0007).
type Worker struct {
	client  *Client
	handler Handler

	// listenConfig is how the listener opens the connection it holds outside
	// the pool. LISTEN belongs to a session, so a pooled connection handed
	// back between notifications would stop being the one that is listening.
	listenConfig *pgx.ConnConfig
}

// NewWorker builds the loop for one kind. The caller owns the pool.
func NewWorker(pool *pgxpool.Pool, cfg Config, handler Handler) (*Worker, error) {
	client, err := New(pool, cfg)
	if err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, fmt.Errorf("%w: a %s worker has no handler", ErrInvalidJob, client.cfg.Kind.Name)
	}
	return &Worker{
		client:       client,
		handler:      handler,
		listenConfig: pool.Config().ConnConfig.Copy(),
	}, nil
}

// Client is the queue the worker runs on, for the caller that also wants to
// read its stats or list its failures.
func (w *Worker) Client() *Client { return w.client }

// Run works the kind's queue until ctx is cancelled, and returns nil when it
// stops that way: a worker asked to stop has not failed. It returns an error
// only for something it cannot work through — a database that stays
// unreachable is not one of those, because the polling floor is what it falls
// back to.
func (w *Worker) Run(ctx context.Context) error {
	cfg := w.client.cfg
	ctx = telemetry.With(ctx, "job_kind", cfg.Kind.Name)
	log := telemetry.Logger(ctx)

	// The listener is this loop's, so this loop cancels it and waits for it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wake := make(chan struct{}, 1)
	var listener sync.WaitGroup
	listener.Add(1)
	go func() {
		defer listener.Done()
		w.listen(ctx, wake)
	}()
	defer listener.Wait()

	poll := time.NewTicker(cfg.PollInterval)
	defer poll.Stop()
	maintain := time.NewTicker(cfg.MaintenanceInterval)
	defer maintain.Stop()

	log.InfoContext(ctx, "queue worker started",
		"concurrency", cfg.Concurrency, "serialized", cfg.Kind.Serialized,
		"poll_interval", cfg.PollInterval.String(), "lease", cfg.Lease.String())

	for {
		// Drain what is due before going back to sleep: one notification can
		// stand for many jobs, because Postgres collapses identical
		// notifications from one transaction.
		for ctx.Err() == nil {
			jobs, err := w.client.Claim(ctx)
			if err != nil {
				if ctx.Err() == nil {
					// Waiting for the next tick is the backoff: hammering a
					// database that is refusing us helps nobody.
					log.ErrorContext(ctx, "claiming jobs failed", "error", err)
				}
				break
			}
			if len(jobs) == 0 {
				break
			}
			w.runAll(ctx, jobs)
		}

		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "queue worker stopped")
			return nil
		case <-wake:
		case <-poll.C:
		case <-maintain.C:
			w.maintain(ctx)
		}
	}
}

// runAll runs a claimed batch and waits for it. Claiming again only once the
// batch is done keeps the number of jobs in flight at Concurrency without a
// pool of its own, and a batch is one claim round trip rather than one per
// job.
func (w *Worker) runAll(ctx context.Context, jobs []Job) {
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runOne(ctx, job)
		}()
	}
	wg.Wait()
}

// reportTimeout bounds how long recording an outcome may take once the handler
// has returned. It is a detached context: a worker shutting down still says
// what happened to the job it was running, rather than leaving a row for the
// reclaimer to work out.
const reportTimeout = 10 * time.Second

// runOne runs one job with its lease kept alive, and records what happened.
func (w *Worker) runOne(ctx context.Context, job Job) {
	ctx = telemetry.With(ctx, "job_id", job.ID, "job_attempt", job.Attempt)
	log := telemetry.Logger(ctx)

	run, lost := context.WithCancel(ctx)
	defer lost()
	var lease sync.WaitGroup
	lease.Add(1)
	go func() {
		defer lease.Done()
		w.keepLease(run, job, lost)
	}()

	err := w.handler(run, job)
	// The heartbeat is this run's, and the run is over.
	lost()
	lease.Wait()

	report, done := context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
	defer done()

	if err == nil {
		applied, cerr := w.client.Complete(report, job)
		switch {
		case cerr != nil:
			log.ErrorContext(report, "recording a finished job failed", "error", cerr)
		case !applied:
			log.WarnContext(report, "a finished job had already been reclaimed, so it will run again")
		default:
			log.DebugContext(report, "job done")
		}
		return
	}

	state, retry, ferr := w.client.Fail(report, job, err)
	switch {
	case ferr != nil:
		log.ErrorContext(report, "recording a failed job failed", "error", ferr, "cause", err.Error())
	case state == "":
		log.WarnContext(report, "a failed job had already been reclaimed, so its outcome was dropped", "cause", err.Error())
	case state == StateFailed:
		log.ErrorContext(report, "job failed for good", "error", err, "attempts", job.Attempt)
	default:
		log.WarnContext(report, "job failed and will be retried", "error", err, "retry_in", retry.In.String())
	}
}

// keepLease renews the job's lease while it runs, and cancels the run when the
// lease turns out to be somebody else's: a reclaimed job is already running
// elsewhere, and two runs writing the same L1 document is the thing the lease
// exists to make rare.
func (w *Worker) keepLease(ctx context.Context, job Job, lost func()) {
	log := telemetry.Logger(ctx)
	tick := time.NewTicker(w.client.cfg.HeartbeatInterval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		held, err := w.client.Heartbeat(ctx, job)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				// Not fatal on its own: the lease still has time on it, and
				// the next tick may reach the database.
				log.WarnContext(ctx, "renewing a job lease failed", "error", err)
			}
		case !held:
			log.WarnContext(ctx, "a job lease was lost and the job reclaimed; cancelling this run")
			lost()
			return
		}
	}
}

// maintain is the housekeeping a running worker does for its own kind:
// reclaiming what dead workers left running, and deleting what is finished and
// past retention.
func (w *Worker) maintain(ctx context.Context) {
	log := telemetry.Logger(ctx)
	if reclaimed, err := w.client.Reclaim(ctx); err != nil {
		if ctx.Err() == nil {
			log.ErrorContext(ctx, "reclaiming expired job leases failed", "error", err)
		}
	} else if reclaimed.Total() > 0 {
		log.WarnContext(ctx, "reclaimed jobs whose leases expired",
			"requeued", reclaimed.Pending, "failed", reclaimed.Failed)
	}
	if purged, err := w.client.Purge(ctx); err != nil {
		if ctx.Err() == nil {
			log.ErrorContext(ctx, "purging finished jobs failed", "error", err)
		}
	} else if purged > 0 {
		log.DebugContext(ctx, "purged finished jobs", "count", purged)
	}
}

// listen wakes the loop when a job of this kind is committed. It reconnects
// for as long as the worker runs: the polling floor is what keeps a listener
// that cannot connect from being an outage, so a failure here is logged and
// retried rather than returned.
func (w *Worker) listen(ctx context.Context, wake chan<- struct{}) {
	log := telemetry.Logger(ctx)
	for ctx.Err() == nil {
		if err := w.listenOnce(ctx, wake); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "the queue listener lost its connection; jobs will wait for the polling floor",
				"error", err, "poll_interval", w.client.cfg.PollInterval.String())
			select {
			case <-ctx.Done():
			case <-time.After(w.client.cfg.PollInterval):
			}
		}
	}
}

// listenOnce holds one connection and forwards its notifications until it
// breaks or the worker stops.
func (w *Worker) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	channel := w.client.cfg.Kind.Channel()
	conn, err := pgx.ConnectConfig(ctx, w.listenConfig)
	if err != nil {
		return fmt.Errorf("connecting the %s listener: %w", channel, err)
	}
	defer func() {
		// Closing takes a context of its own: the worker's is cancelled by the
		// time this runs on the way out.
		closing, done := context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
		defer done()
		conn.Close(closing) //nolint:errcheck // there is nothing to do about a connection that will not close
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return fmt.Errorf("listening on %s: %w", channel, err)
	}
	// Anything enqueued while there was no listener was announced to nobody.
	// Waking now costs one empty claim and saves a poll interval of latency.
	signal(wake)

	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("waiting for a notification on %s: %w", channel, err)
		}
		signal(wake)
	}
}

// signal wakes the loop without blocking. The channel holds one wake-up,
// because a second one before the loop has looked changes nothing: it claims
// everything that is due either way.
func signal(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}
