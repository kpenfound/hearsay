package connector

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/hearsay/internal/telemetry"
)

// The runtime's own cadences. A source config says how often it wants to be
// polled; these are what the runtime puts around that (docs/connector-contract.md).
const (
	// DefaultMinRefresh is the floor under a source's refresh: the runtime
	// never polls a source more often than this, whatever config asks for. A
	// source that wants to be quicker than this wants a webhook.
	DefaultMinRefresh = 30 * time.Second
	// DefaultRefresh is the cadence for a source whose config names none.
	DefaultRefresh = 5 * time.Minute
	// DefaultMaxBackoff caps the delay after a failed poll or backfill call. It
	// is a cap and not a cadence: a source polled less often than this is
	// retried on its own cadence rather than dragged up to this one.
	DefaultMaxBackoff = 15 * time.Minute
	// DefaultShutdown is how long [Runtime.Close] gives the connectors to stop
	// after the process has been asked to shut down.
	DefaultShutdown = 10 * time.Second
)

// jitterFraction is how much later than its due time a tick may run: a tenth of
// the interval, drawn uniformly, and never earlier.
//
// One-sided, unlike the queue's retry jitter, because the interval is a promise
// to the source — a floor of 30 seconds that a jittered tick could undercut is
// not a floor — and because polls do not arrive in a herd the way the retries
// of jobs a provider rate-limited together do. What it is for is two sources,
// or two replicas, whose ticks would otherwise stay in lockstep forever.
const jitterFraction = 10

// HookPrefix is the path prefix the runtime owns for push connectors. A
// [Pusher]'s handler is mounted at [HookPath], and everything under the prefix
// belongs to the runtime rather than to the service hosting it.
const HookPrefix = "/hooks/"

// HookPath is where a source's push handler is mounted. It is the URL the
// source is configured to deliver to, so it is derived from the source id and
// nothing else: a source id is lowercase letters, digits, `-` and `_`, so it
// needs no escaping.
func HookPath(source string) string { return HookPrefix + source }

// RuntimeOptions is everything a [Runtime] is built from.
type RuntimeOptions struct {
	// Sources are the configured sources this runtime hosts, already narrowed
	// to the selection the process was started with. Their secrets still name
	// environment variables; the runtime resolves them.
	Sources []SourceConfig
	// Registry is what builds a connector for each source. A source whose type
	// no factory claims is a startup failure.
	Registry *Registry
	// Sink is where events land, once the gate has passed them. It is the L0
	// store in a process and a [Recorder] in a test. It is required.
	Sink Sink
	// Cursors is where backfill positions are kept. A nil store drives no
	// backfill at all: rather than walk a source's history from the beginning
	// on every restart, the runtime says so and polls only.
	Cursors CursorStore
	// Lookup resolves the environment variable a secret names. Nil is
	// [os.LookupEnv], which is what a process uses; a test passes its own
	// rather than setting variables on the process it shares with every other
	// test.
	Lookup func(string) (string, bool)
	// Cadence is the runtime's own timings. Its zero value is the defaults.
	Cadence Cadence
}

// Cadence is what the runtime puts around a source's own refresh: the floor
// under it, the fallback for a source that names none, the cap on backing off
// when a call fails, and how long connectors get to shut down.
//
// Every field may be left zero, and everything that reads one fills the
// defaults in first, so only a value somebody set can be wrong.
type Cadence struct {
	// MinRefresh is the floor under a source's refresh. Zero is
	// [DefaultMinRefresh].
	MinRefresh time.Duration
	// Refresh is the cadence for a source whose config names none. Zero is
	// [DefaultRefresh].
	Refresh time.Duration
	// MaxBackoff caps the delay after a failed call. Zero is
	// [DefaultMaxBackoff].
	MaxBackoff time.Duration
	// Shutdown is how long the connectors get to close. Zero is
	// [DefaultShutdown].
	Shutdown time.Duration
}

func (c Cadence) withDefaults() Cadence {
	if c.MinRefresh <= 0 {
		c.MinRefresh = DefaultMinRefresh
	}
	if c.Refresh <= 0 {
		c.Refresh = DefaultRefresh
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = DefaultMaxBackoff
	}
	if c.Shutdown <= 0 {
		c.Shutdown = DefaultShutdown
	}
	return c
}

// Interval is how often a source is polled: what it asked for, under the
// runtime's floor, and the fallback for a source that asked for nothing. It is
// exported because "why is this source polled every five minutes when config
// says thirty seconds" is a question an operator asks of a running process.
//
// The tick itself is jittered on top of this; the interval is not, because it
// is also the base [Cadence.Backoff] doubles.
func (c Cadence) Interval(src SourceConfig) time.Duration {
	c = c.withDefaults()
	d := src.Refresh
	if d <= 0 {
		d = c.Refresh
	}
	return max(d, c.MinRefresh)
}

// Backoff is how long to wait after fails consecutive failures at an interval:
// the interval doubled once per failure after the first, capped, and jittered
// like every other tick. Zero or one failure is the interval itself.
//
// The cap never pulls a slow source up to it: a source polled hourly that is
// failing is retried hourly, not every fifteen minutes.
func (c Cadence) Backoff(interval time.Duration, fails int) time.Duration {
	c = c.withDefaults()
	limit := max(c.MaxBackoff, interval)
	d := interval
	// Doubling in a loop rather than by shifting, so that a long-failing source
	// cannot shift its way to a negative duration.
	for i := 1; i < fails && d < limit; i++ {
		d *= 2
	}
	if d > limit || d <= 0 {
		d = limit
	}
	return jitter(d)
}

// jitter spreads a tick over the [jitterFraction] of the interval after it is
// due.
func jitter(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d + time.Duration(rand.Int64N(int64(d)/jitterFraction+1))
}

// ResolveSecrets returns the source config a connector is built from: the same
// config with every secret's environment variable read. Configuration names a
// secret and the runtime supplies its value (docs/config.md), so this is the
// one place the two meet.
//
// A variable that is unset or empty is a startup failure, not a connector that
// will fail on its first call: a source that cannot start is a startup failure
// (docs/connector-contract.md). The error names the variable and never its
// value.
func ResolveSecrets(src SourceConfig, lookup func(string) (string, bool)) (SourceConfig, error) {
	if len(src.Secrets) == 0 {
		return src, nil
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	resolved := make(map[string]string, len(src.Secrets))
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(src.Secrets)) {
		env := src.Secrets[name]
		value, ok := lookup(env)
		if !ok || value == "" {
			missing = append(missing, fmt.Sprintf("%s (%s)", name, env))
			continue
		}
		resolved[name] = value
	}
	if len(missing) > 0 {
		return SourceConfig{}, fmt.Errorf("source %q: no value in the environment for %v", src.ID, missing)
	}
	src.Secrets = resolved
	return src, nil
}

// hosted is one connector the runtime is running, and what the runtime knows
// about it that the connector does not: what it writes through, how its poll
// loop is going, and where its backfill has got to.
type hosted struct {
	src  SourceConfig
	conn Connector
	gate *Gate

	pollFails     atomic.Int64
	backfillFails atomic.Int64
	backfillDone  atomic.Bool
	backfilled    atomic.Int64
}

// Runtime hosts a set of connectors: it builds one per configured source, polls
// the pollers on their own cadence, drives the backfillers through their
// cursors, mounts the pushers' handlers, and reports what all of them think of
// themselves.
//
// It is the supervision the connector contract leaves to Hearsay. What may be
// written is the [Gate]'s, and every connector here writes through one.
type Runtime struct {
	opts  RuntimeOptions
	conns []*hosted
	mux   *http.ServeMux

	closeOnce sync.Once
	closeErr  error
}

// NewRuntime builds a connector for every source and returns the runtime that
// hosts them. A source that cannot be built — an unknown type, a secret that is
// not in the environment, settings a connector rejects — is a startup failure,
// and every connector already built is closed before the error is returned.
func NewRuntime(ctx context.Context, opts RuntimeOptions) (*Runtime, error) {
	if opts.Lookup == nil {
		opts.Lookup = os.LookupEnv
	}
	opts.Cadence = opts.Cadence.withDefaults()
	if opts.Sink == nil {
		return nil, errors.New("the connector runtime needs a sink to write events to")
	}
	if opts.Registry == nil {
		return nil, errors.New("the connector runtime needs a registry to build connectors from")
	}

	allow := NewAllowlist(opts.Sources...)
	r := &Runtime{opts: opts, mux: http.NewServeMux()}
	seen := make(map[string]bool, len(opts.Sources))
	for _, src := range opts.Sources {
		// Two connectors for one source id would share a gate's allowlist, two
		// backfills would share a cursor, and two push handlers would claim one
		// path — which is a panic rather than an error, so this is checked
		// before anything is built.
		if seen[src.ID] {
			return nil, r.abandon(ctx, fmt.Errorf("source %q is configured twice", src.ID))
		}
		seen[src.ID] = true
		resolved, err := ResolveSecrets(src, opts.Lookup)
		if err != nil {
			return nil, r.abandon(ctx, err)
		}
		conn, err := opts.Registry.New(ctx, resolved)
		if err != nil {
			return nil, r.abandon(ctx, err)
		}
		h := &hosted{src: src, conn: conn, gate: NewGate(opts.Sink, src.ID, conn.Describe(), allow)}
		r.conns = append(r.conns, h)
		if pusher, ok := conn.(Pusher); ok {
			r.mux.Handle(HookPath(src.ID), pusher.Handler(h.gate))
		}
	}
	return r, nil
}

// abandon closes the connectors a failed NewRuntime had already built. Nothing
// else holds them, so nothing else can.
func (r *Runtime) abandon(ctx context.Context, reason error) error {
	if err := r.Close(ctx); err != nil {
		return errors.Join(reason, err)
	}
	return reason
}

// Handler serves the push connectors' handlers, each at [HookPath] for its
// source. Everything under [HookPrefix] is the runtime's, so a service mounts
// this whole handler at that prefix rather than reaching for individual paths.
func (r *Runtime) Handler() http.Handler { return r.mux }

// Sources is the id of every source this runtime hosts, in config order.
func (r *Runtime) Sources() []string {
	ids := make([]string, 0, len(r.conns))
	for _, h := range r.conns {
		ids = append(ids, h.src.ID)
	}
	return ids
}

// Run drives every connector until ctx is cancelled, and closes them all before
// it returns. It returns nil when it stopped that way: a connector that fails
// is a health status, not a reason to stop the process, because the other
// sources are still ingesting.
//
// A runtime that was built and is never run must still be closed.
func (r *Runtime) Run(ctx context.Context) error {
	log := telemetry.Logger(ctx)
	if len(r.conns) == 0 {
		log.WarnContext(ctx, "no sources configured: nothing is ingested, pass --config")
	}

	var wg sync.WaitGroup
	for _, h := range r.conns {
		hctx := telemetry.With(ctx, "source", h.src.ID, "connector", h.src.Type)
		if poller, ok := h.conn.(Poller); ok {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.poll(hctx, h, poller)
			}()
		}
		backfiller, ok := h.conn.(Backfiller)
		switch {
		case !ok:
		case r.opts.Cursors == nil:
			// Driving a backfill with nowhere to keep the cursor would walk the
			// whole of a source's history again on every restart, which is the
			// expensive half of ingest.
			telemetry.Logger(hctx).WarnContext(hctx, "no cursor store: history is not backfilled")
		default:
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.backfill(hctx, h, backfiller)
			}()
		}
	}
	wg.Wait()
	// Every loop above returns when ctx is done, but a backfill that reaches
	// the end of history returns before that and a push-only source has no loop
	// at all: the process still hosts their handlers and their health until it
	// is asked to stop.
	<-ctx.Done()

	// The context that stopped the loops is done, and a connector's Close is
	// entitled to a context that is not: closing is what a connector does with
	// the goroutines and sockets it owns.
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.opts.Cadence.Shutdown)
	defer cancel()
	return r.Close(closeCtx)
}

// Close closes every connector, once. Every connector is closed even when one
// of them fails, and the failures are joined: one connector that will not shut
// down must not keep the others open.
func (r *Runtime) Close(ctx context.Context) error {
	r.closeOnce.Do(func() {
		errs := make([]error, 0, len(r.conns))
		for _, h := range r.conns {
			if err := h.conn.Close(ctx); err != nil {
				errs = append(errs, fmt.Errorf("closing the connector for source %q: %w", h.src.ID, err))
			}
		}
		r.closeErr = errors.Join(errs...)
	})
	return r.closeErr
}

// poll calls Poll on the source's cadence, and never concurrently with itself:
// this goroutine is the only caller, which is what lets a poller keep its
// position in memory without locking.
func (r *Runtime) poll(ctx context.Context, h *hosted, poller Poller) {
	log := telemetry.Logger(ctx)
	interval := r.opts.Cadence.Interval(h.src)
	log.InfoContext(ctx, "polling", "refresh", interval.String())

	// The first poll is immediate but for the jitter, so that a process that
	// has just started ingests rather than waiting out a refresh interval.
	wait := jitter(0)
	fails := 0
	for {
		if !sleep(ctx, wait) {
			log.InfoContext(ctx, "polling stopped")
			return
		}

		err := poller.Poll(ctx, h.gate)
		switch {
		case err != nil && ctx.Err() != nil:
			// Cancelled mid-poll: the process is shutting down, not the source
			// failing.
			log.InfoContext(ctx, "polling stopped")
			return
		case err != nil:
			fails++
			h.pollFails.Store(int64(fails))
			wait = r.opts.Cadence.Backoff(interval, fails)
			log.WarnContext(ctx, "poll failed, retrying", "error", err, "failures", fails, "retry_in", wait.String())
		default:
			fails = 0
			h.pollFails.Store(0)
			wait = jitter(interval)
		}
	}
}

// backfill walks a source's history: Backfill against the stored cursor, and
// the cursor it returns stored before the next call, so that a restart resumes
// where this left off rather than at the beginning.
func (r *Runtime) backfill(ctx context.Context, h *hosted, backfiller Backfiller) {
	log := telemetry.Logger(ctx)
	interval := r.opts.Cadence.Interval(h.src)

	state, err := r.load(ctx, h, interval)
	if err != nil {
		return
	}
	// What health says about the backfill is what the stored position says,
	// including on a process that did none of the walking: a source walked in an
	// earlier process is walked, and readiness that said otherwise would have an
	// operator conclude a deploy had lost it.
	h.backfilled.Store(state.Events)
	h.backfillDone.Store(state.Done)
	if state.Done {
		log.InfoContext(ctx, "history is already backfilled", "events", state.Events)
		return
	}

	fails := 0
	for {
		res, err := backfiller.Backfill(ctx, h.gate, state.Cursor)
		if ctx.Err() != nil {
			log.InfoContext(ctx, "backfill stopped")
			return
		}
		if err == nil && !res.Done && res.Events == 0 && res.Next == state.Cursor {
			// A call that emitted nothing and handed back the cursor it was
			// given would be called with that cursor forever. It is the
			// connector breaking the contract — one call does a bounded amount
			// of work and returns Done when there is no more — so it is treated
			// as a failure rather than as the end of history, which would
			// record a walk that never happened.
			err = errors.New("backfill made no progress: it emitted nothing and returned the cursor it was given")
		}
		if err != nil {
			fails++
			h.backfillFails.Store(int64(fails))
			wait := r.opts.Cadence.Backoff(interval, fails)
			log.WarnContext(ctx, "backfill failed, retrying", "error", err, "failures", fails, "retry_in", wait.String())
			if !sleep(ctx, wait) {
				log.InfoContext(ctx, "backfill stopped")
				return
			}
			continue
		}

		next := BackfillState{Cursor: res.Next, Done: res.Done, Events: state.Events + int64(res.Events)}
		if res.Done {
			// Next is ignored when Done is set, so the position stays where the
			// last call that meant one left it.
			next.Cursor = state.Cursor
		}
		if err := r.save(ctx, h, next, interval); err != nil {
			return
		}
		// The count clears here, when a whole iteration has worked — the call
		// and the position it produced — rather than between the two. (A save
		// that is retrying keeps the count itself, so the two orders report the
		// same thing; this one is the one that reads as what it means.)
		fails = 0
		h.backfillFails.Store(0)
		state = next
		h.backfilled.Store(state.Events)
		if state.Done {
			h.backfillDone.Store(true)
			log.InfoContext(ctx, "history backfilled", "events", state.Events)
			return
		}
	}
}

// load reads the source's position, retrying until it works or the process
// stops: a database that is briefly unreachable is not a reason to skip a
// source's history for the lifetime of the process.
func (r *Runtime) load(ctx context.Context, h *hosted, interval time.Duration) (BackfillState, error) {
	log := telemetry.Logger(ctx)
	for fails := 1; ; fails++ {
		state, err := r.opts.Cursors.Load(ctx, h.src.ID)
		if err == nil {
			// A read that worked clears the count, however many attempts it
			// took. The loop below clears it too, after a call and its save —
			// but a source whose history is already walked never reaches the
			// loop, so a transient failure here would be the number health
			// reported for the life of the process.
			h.backfillFails.Store(0)
			return state, nil
		}
		if ctx.Err() != nil {
			return BackfillState{}, ctx.Err()
		}
		// Counted like every other backfill failure: a source whose position
		// cannot even be read is one whose history is not being walked, and
		// health has to say so rather than showing a source with nothing left
		// to do.
		h.backfillFails.Store(int64(fails))
		wait := r.opts.Cadence.Backoff(interval, fails)
		log.WarnContext(ctx, "reading the backfill cursor failed, retrying", "error", err, "failures", fails, "retry_in", wait.String())
		if !sleep(ctx, wait) {
			return BackfillState{}, ctx.Err()
		}
	}
}

// save stores the position, retrying until it works or the process stops. It
// retries rather than carrying on because carrying on is what loses the
// position: the next call would move the cursor past a page whose position was
// never written down.
func (r *Runtime) save(ctx context.Context, h *hosted, state BackfillState, interval time.Duration) error {
	log := telemetry.Logger(ctx)
	for fails := 1; ; fails++ {
		err := r.opts.Cursors.Save(ctx, h.src.ID, state)
		if err == nil {
			return nil
		}
		h.backfillFails.Store(int64(fails))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := r.opts.Cadence.Backoff(interval, fails)
		log.WarnContext(ctx, "saving the backfill cursor failed, retrying", "error", err, "failures", fails, "retry_in", wait.String())
		if !sleep(ctx, wait) {
			return ctx.Err()
		}
	}
}

// sleep waits, and reports whether the wait finished rather than the context.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RuntimeHealth is what the process running connectors reports about them: the
// worst status among them, and each one's own account of itself.
//
// It is served more widely than L0, so it carries no credentials, no event text
// and no personal data — the same rule the contract puts on [Health.Detail],
// which is where a connector's own words end up.
type RuntimeHealth struct {
	// Status is the worst of the sources' statuses: failed if any source has
	// failed, degraded if any is degraded, ok otherwise. A runtime hosting no
	// source is ok — a process with nothing configured is not broken.
	Status HealthStatus `json:"status"`
	// Sources is one entry per hosted source, in config order.
	Sources []SourceHealth `json:"sources"`
}

// SourceHealth is one hosted connector: what it says about itself, and what the
// runtime has seen of it.
type SourceHealth struct {
	// Source is the source id, and Type the connector type serving it.
	Source string `json:"source"`
	Type   string `json:"type"`
	// Status, Detail and LastEventAt are the connector's own [Health],
	// unchanged.
	Status      HealthStatus `json:"status"`
	Detail      string       `json:"detail,omitempty"`
	LastEventAt time.Time    `json:"last_event_at,omitzero"`
	// Dropped is how many events the ingest allowlist has refused, which is the
	// answer to "why is this source quiet" that health would otherwise not
	// have: a container nobody put in config is not an error anywhere.
	Dropped int64 `json:"dropped"`
	// PollFailures is how many times in a row the source's poll has failed, and
	// zero once one succeeds. The failure itself is in the log; a count is what
	// health can carry without repeating a message from a source.
	PollFailures int64 `json:"poll_failures"`
	// BackfillFailures is how many times in a row the source's backfill has
	// failed — the call itself, or the store that will not take the position it
	// returned — and zero once one works. A backfill retries forever, so
	// without this a source whose history is stuck looks like one that has no
	// history left to walk.
	BackfillFailures int64 `json:"backfill_failures"`
	// BackfillDone reports that history is exhausted, and Backfilled is how
	// many events it has emitted. Both are what the *stored* position says, so
	// a process that resumed a finished backfill reports it as finished.
	BackfillDone bool  `json:"backfill_done"`
	Backfilled   int64 `json:"backfilled"`
}

// Health is every hosted connector's own Health, aggregated. It makes no
// network call — the contract forbids one on this path — so it is cheap enough
// to serve on every request.
func (r *Runtime) Health(ctx context.Context) RuntimeHealth {
	out := RuntimeHealth{Status: HealthOK, Sources: make([]SourceHealth, 0, len(r.conns))}
	for _, h := range r.conns {
		health := h.conn.Health(ctx)
		out.Sources = append(out.Sources, SourceHealth{
			Source:           h.src.ID,
			Type:             h.src.Type,
			Status:           health.Status,
			Detail:           health.Detail,
			LastEventAt:      health.LastEventAt,
			Dropped:          h.gate.Dropped(),
			PollFailures:     h.pollFails.Load(),
			BackfillFailures: h.backfillFails.Load(),
			BackfillDone:     h.backfillDone.Load(),
			Backfilled:       h.backfilled.Load(),
		})
		out.Status = worse(out.Status, health.Status)
	}
	return out
}

// worse is the worse of two statuses. An unknown status counts as failed: a
// connector reporting something the runtime does not understand is not one to
// call healthy.
func worse(a, b HealthStatus) HealthStatus {
	if rank(b) > rank(a) {
		return known(b)
	}
	return known(a)
}

// known maps a status the contract does not define onto failed, so that a
// connector reporting something nobody understands is not reported as working.
func known(s HealthStatus) HealthStatus {
	switch s {
	case HealthOK, HealthDegraded, HealthFailed:
		return s
	default:
		return HealthFailed
	}
}

func rank(s HealthStatus) int {
	switch s {
	case HealthOK:
		return 0
	case HealthDegraded:
		return 1
	case HealthFailed:
		return 2
	default:
		return 3
	}
}
