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
// document text, no prompt and no completion.
//
// A handler must be safe to run twice on the same job. Delivery is
// at-least-once: a worker that dies mid-job leaves a job that is reclaimed and
// run again, and that is the semantics the queue chose knowing both of
// Hearsay's workers are idempotent. The exception is a job whose target has
// another job pending by then: that one does the work, and this one is marked
// done as superseded rather than run again.
//
// The context is cancelled when the worker is shutting down and when the
// job's lease has been lost to another worker, which is the signal to stop
// working on it: another worker is already running it.
type Handler func(ctx context.Context, job Job) error

// Worker is the loop over a [Client]: claim, run, report, repeat. It wakes on
// a notification for its kind and, failing that, on the polling floor, so a
// missed notification delays a job rather than stranding it.
type Worker struct {
	client  *Client
	handler Handler

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

// Run works the kind's queue until ctx is cancelled, and returns nil when it
// stops that way: a worker asked to stop has not failed. It returns an error
// only for something it cannot work through — a database that stays
// unreachable is not one of those, because the polling floor is what it falls
// back to.
func (w *Worker) Run(ctx context.Context) error {
	cfg := w.client.cfg
	ctx = telemetry.With(ctx, "job_kind", cfg.Kind.Name)
	log := telemetry.Logger(ctx)

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
		for ctx.Err() == nil {
			select {
			case <-maintain.C:
				w.maintain(ctx)
			default:
			}

			jobs, err := w.client.Claim(ctx)
			if err != nil {
				if ctx.Err() == nil {
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

const reportTimeout = 10 * time.Second

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
	case state == StateDone:
		log.InfoContext(report, "a failed job was superseded by one already pending for its target, so it will not be retried",
			"error", err)
	default:
		log.WarnContext(report, "job failed and will be retried", "error", err, "retry_in", retry.In.String())
	}
}

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
				log.WarnContext(ctx, "renewing a job lease failed", "error", err)
			}
		case !held:
			log.WarnContext(ctx, "a job lease was lost and the job reclaimed; cancelling this run")
			lost()
			return
		}
	}
}

func (w *Worker) maintain(ctx context.Context) {
	log := telemetry.Logger(ctx)
	if reclaimed, err := w.client.Reclaim(ctx); err != nil {
		if ctx.Err() == nil {
			log.ErrorContext(ctx, "reclaiming expired job leases failed", "error", err)
		}
	} else if reclaimed.Total() > 0 {
		log.WarnContext(ctx, "reclaimed jobs whose leases expired",
			"requeued", reclaimed.Pending, "failed", reclaimed.Failed, "superseded", reclaimed.Superseded)
	}
	if purged, err := w.client.Purge(ctx); err != nil {
		if ctx.Err() == nil {
			log.ErrorContext(ctx, "purging finished jobs failed", "error", err)
		}
	} else if purged > 0 {
		log.DebugContext(ctx, "purged finished jobs", "count", purged)
	}
}

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

func (w *Worker) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	channel := w.client.cfg.Kind.Channel()
	conn, err := pgx.ConnectConfig(ctx, w.listenConfig)
	if err != nil {
		return fmt.Errorf("connecting the %s listener: %w", channel, err)
	}
	defer func() {
		closing, done := context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
		defer done()
		conn.Close(closing) //nolint:errcheck // there is nothing to do about a connection that will not close
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return fmt.Errorf("listening on %s: %w", channel, err)
	}
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

func signal(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}
