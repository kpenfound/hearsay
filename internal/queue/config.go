package queue

import (
	"fmt"
	"math/rand/v2"
	"time"
)

// The defaults a zero Config fills in. They are the shape of the work
// ADR-0007 describes: jobs that are model calls measured in seconds, on a
// queue that is thousands of events a day rather than millions a minute.
const (
	// DefaultLease is how long a claim is believed without a heartbeat.
	DefaultLease = time.Minute
	// DefaultPollInterval is the polling floor: what a worker falls back to
	// when a notification is missed. It bounds how late a job can be, so it is
	// a few seconds rather than a minute.
	DefaultPollInterval = 5 * time.Second
	// DefaultMaxAttempts is how many runs a job gets before it fails for good.
	DefaultMaxAttempts = 5
	// DefaultBackoff is the first retry's delay, doubled per attempt.
	DefaultBackoff = 5 * time.Second
	// DefaultMaxBackoff caps it.
	DefaultMaxBackoff = 5 * time.Minute
	// DefaultRetention is how long a done job is kept before it is deleted.
	// Failed jobs are never deleted.
	DefaultRetention = 24 * time.Hour
	// DefaultMaintenanceInterval is how often a worker reclaims expired leases
	// and purges what it has finished.
	DefaultMaintenanceInterval = time.Minute
)

// minHeartbeatInterval keeps a very short lease from turning into a heartbeat
// per millisecond, which is what a test with a 30ms lease would otherwise ask
// for.
const minHeartbeatInterval = 50 * time.Millisecond

// Config is the consumer side of one job kind: what a [Client]'s operations
// use and what the [Worker] loop over them runs on. Producers do not need one
// — [Enqueue] takes a [Request] and the caller's transaction.
//
// Every field may be left zero, and every method here fills the defaults in
// before it reads one: a zero Config for a valid kind is a working queue, and
// only a value somebody set can be refused.
type Config struct {
	// Kind is the job kind this client works on. A client, a worker and a
	// channel are all per kind.
	Kind Kind

	// Concurrency is how many jobs one claim takes and one worker runs at
	// once. It must be 1 on a serialized kind: batching there would claim
	// several jobs sharing a serial key in one statement, all of them checked
	// against a snapshot in which none of them is running yet, which is the
	// exact failure ADR-0007 works through.
	Concurrency int

	// Lease is how long a claimed job is another worker's to take back. The
	// worker heartbeats at a third of it while the handler runs, so it is a
	// bound on how long a dead worker's job sits, not on how long a job may
	// take.
	Lease time.Duration

	// PollInterval is the polling floor. Workers wake on a notification; this
	// is what makes a missed one a delay rather than a stranded job.
	PollInterval time.Duration

	// MaxAttempts is how many runs a job gets. The run that reaches it moves
	// the job to failed, where it stays.
	MaxAttempts int

	// Backoff is the delay before the second attempt, doubled for each
	// attempt after it and capped at MaxBackoff. The delay actually used is
	// drawn uniformly from below that, so retries that failed together do not
	// return together.
	Backoff    time.Duration
	MaxBackoff time.Duration

	// Retention is how long a done job is kept. Failed jobs ignore it.
	Retention time.Duration

	// MaintenanceInterval is how often a running worker reclaims this kind's
	// expired leases and purges its done jobs.
	MaintenanceInterval time.Duration
}

// withDefaults fills in what was left zero.
func (c Config) withDefaults() Config {
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.Lease == 0 {
		c.Lease = DefaultLease
	}
	if c.PollInterval == 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.Backoff == 0 {
		c.Backoff = DefaultBackoff
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = DefaultMaxBackoff
	}
	if c.Retention == 0 {
		c.Retention = DefaultRetention
	}
	if c.MaintenanceInterval == 0 {
		c.MaintenanceInterval = DefaultMaintenanceInterval
	}
	return c
}

// Validate reports a configuration the queue refuses, with the defaults filled
// in first: a zero field is a default, and only a value somebody set can be
// wrong. It refuses rather than rounds: a concurrency a serialized kind cannot honour is a wrong answer
// nobody has a reason to doubt.
func (c Config) Validate() error {
	c = c.withDefaults()
	if err := c.Kind.Validate(); err != nil {
		return err
	}
	switch {
	case c.Concurrency < 1:
		return fmt.Errorf("%w: concurrency %d for kind %s; it is how many jobs run at once, so it is at least 1", ErrInvalidJob, c.Concurrency, c.Kind.Name)
	case c.Kind.Serialized && c.Concurrency > 1:
		return fmt.Errorf("%w: kind %s is serialized and claims one job at a time, so concurrency %d cannot be honoured (ADR-0007)", ErrInvalidJob, c.Kind.Name, c.Concurrency)
	case c.Lease <= 0:
		return fmt.Errorf("%w: lease %s for kind %s; a claim that is already expired would be reclaimed under the worker running it", ErrInvalidJob, c.Lease, c.Kind.Name)
	case c.PollInterval <= 0:
		return fmt.Errorf("%w: poll interval %s for kind %s; it is the floor under a missed notification", ErrInvalidJob, c.PollInterval, c.Kind.Name)
	case c.MaxAttempts < 1:
		return fmt.Errorf("%w: max attempts %d for kind %s; a job gets at least one run", ErrInvalidJob, c.MaxAttempts, c.Kind.Name)
	case c.Backoff <= 0:
		return fmt.Errorf("%w: backoff %s for kind %s", ErrInvalidJob, c.Backoff, c.Kind.Name)
	case c.MaxBackoff < c.Backoff:
		return fmt.Errorf("%w: max backoff %s for kind %s is below its backoff %s", ErrInvalidJob, c.MaxBackoff, c.Kind.Name, c.Backoff)
	case c.Retention <= 0:
		return fmt.Errorf("%w: retention %s for kind %s; done jobs are deleted after a window, not immediately", ErrInvalidJob, c.Retention, c.Kind.Name)
	case c.MaintenanceInterval <= 0:
		return fmt.Errorf("%w: maintenance interval %s for kind %s", ErrInvalidJob, c.MaintenanceInterval, c.Kind.Name)
	}
	return nil
}

// HeartbeatInterval is how often a worker renews the lease on a job it is
// running: a third of the lease, so two heartbeats can be lost before anything
// reclaims the job, and never below a floor, so that a short lease does not
// turn into a heartbeat per millisecond.
//
// A lease below three times that floor is therefore renewed no faster than the
// floor, and may be renewed after it has already expired — the job is then
// reclaimed and the worker running it is told so at its next heartbeat. That
// is a lease measured in tens of milliseconds, which is a test's, not a
// deployment's.
func (c Config) HeartbeatInterval() time.Duration {
	c = c.withDefaults()
	d := c.Lease / 3
	if d < minHeartbeatInterval {
		d = minHeartbeatInterval
	}
	return d
}

// RetryDelay is how long a job waits after a failed attempt before it may run
// again: Backoff doubled once per attempt so far, capped at MaxBackoff, and
// then drawn uniformly from everything up to that.
//
// The jitter spans the whole interval rather than a fraction of it, because
// the jobs that fail together are the ones a provider rate-limited together,
// and a retry they all make at the same later instant is the same thundering
// herd one round later. It is what [Client.Fail] applies, and it is exported
// so that an operator reading a job's attempt count can work out when it runs.
func (c Config) RetryDelay(attempt int) time.Duration {
	c = c.withDefaults()
	if attempt < 1 {
		attempt = 1
	}
	d := c.Backoff
	// Doubling in a loop rather than by shifting: an attempt count high enough
	// to overflow a shift is reachable by configuration, and the cap is the
	// answer to both.
	for i := 1; i < attempt && d < c.MaxBackoff; i++ {
		d *= 2
	}
	if d > c.MaxBackoff || d <= 0 {
		d = c.MaxBackoff
	}
	return time.Duration(rand.Int64N(int64(d)) + 1)
}
