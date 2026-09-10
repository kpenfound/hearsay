// Package connectors hosts the source connectors. Connectors write L0 and
// nothing else: they turn a Slack thread, a GitHub webhook or a Drive change
// into events, and every layer above is someone else's job.
//
// One process can host several connectors; which ones it hosts is the
// `--source` selection (ADR-0003). What it hosts them with is
// [connector.Runtime]: this package is the wiring — the configured sources, the
// registry of factories, the L0 store to write to, the cursor store to keep
// backfill positions in — and the HTTP surface the runtime needs, which is the
// push connectors' handlers and the service's health (ADR-0008).
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "connectors"

// DefaultListen is where the service serves the push connectors' handlers and
// its own health when nothing says otherwise. The API's port is 8080; this is
// the next one.
const DefaultListen = ":8081"

// shutdownGrace is how long the HTTP server gets to finish the requests it is
// serving once the process has been asked to stop. A webhook delivery is a
// small POST, so this is about a request in flight rather than about a
// long-running one.
const shutdownGrace = 5 * time.Second

// Deps are the dependencies the process builds and hands to [Run].
type Deps struct {
	// Sources selects which configured connectors to host. Empty means all of
	// them, which is the default. A name that is not a configured source is a
	// startup failure rather than a process that hosts nothing.
	Sources []string
	// Pool is the database (ADR-0004). The caller owns it and closes it. It is
	// what [Sink] and [Deps.Cursors] are built from when they are not supplied.
	Pool *pgxpool.Pool
	// Registry is the connector types this binary can ingest. There is no
	// global registry and no init-time registration, so this is where what a
	// process can ingest is decided (docs/connector-contract.md).
	Registry *connector.Registry
	// Sink is where events land. Nil is the L0 store over [Deps.Pool], which is
	// what a process uses; a test supplies its own.
	Sink connector.Sink
	// Cursors is where backfill positions are kept. Nil is the backfill cursor
	// store over [Deps.Pool].
	Cursors connector.CursorStore
	// Listener is what the HTTP surface is served on. Nil is a listener on
	// [Deps.Listen]; a test passes one bound to port 0 so that it knows the
	// address.
	Listener net.Listener
	// Listen is the address to serve on when [Deps.Listener] is nil. Empty is
	// [DefaultListen].
	Listen string
	// Cadence is what the runtime's own timings are turned down to for a
	// deployment that wants it quieter and for a test that wants it immediate.
	// The zero value is the defaults.
	Cadence connector.Cadence
}

// Run hosts the configured connectors until ctx is cancelled, and returns nil
// when it stops that way.
//
// The runtime and the HTTP surface stop together and in that order: a process
// serving webhooks with no runtime behind them would accept deliveries it
// cannot ingest, and a runtime with no listener cannot be delivered to or asked
// how it is.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	// `hosting`, not `source`: ADR-0008 makes `source` the source an event or a
	// line is about, which is what the runtime puts on its own lines. This is
	// the different fact that one process may host several of them.
	ctx = telemetry.With(ctx, "hosting", Selection(deps.Sources))
	log := telemetry.Logger(ctx)

	sources, err := Select(cfg.Repo.Sources, deps.Sources)
	if err != nil {
		return err
	}
	sink, cursors, err := stores(deps)
	if err != nil {
		return err
	}
	registry := deps.Registry
	if registry == nil {
		registry = connector.NewRegistry()
	}

	listener, err := listen(ctx, deps)
	if err != nil {
		return stoppingEarly(ctx, err)
	}
	runtime, err := connector.NewRuntime(ctx, connector.RuntimeOptions{
		Sources:  sources,
		Registry: registry,
		Sink:     sink,
		Cursors:  cursors,
		Cadence:  deps.Cadence,
	})
	if err != nil {
		return errors.Join(err, listener.Close())
	}

	log.InfoContext(ctx, "connectors started",
		"sources", len(sources), "listen", listener.Addr().String(), "config_digest", cfg.Repo.Digest)

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	server := &http.Server{
		Handler:           handler(runtime, deps.Pool),
		ReadHeaderTimeout: 10 * time.Second,
		// Not the cancellable context: cancelling it would cancel every request
		// in flight the instant the process is asked to stop, which is the
		// opposite of what the shutdown grace period is for. What a request
		// wants from it is the logger.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	// The runtime's context is not a child of the one that stops the listener:
	// it is cancelled by the HTTP goroutine once the server has drained, which
	// is what puts the two shutdowns in order.
	runtimeCtx, stopRuntime := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRuntime()
	errs := make([]error, 0, 2)
	var mu sync.Mutex
	fail := func(name string, err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// The listener stops first and the connectors when it has drained: a
		// delivery still being served is inside a connector's Handler, and
		// Close is a connector's chance to let go of what its handlers use.
		defer stopRuntime()
		fail("http", serve(ctx, server, listener))
	}()
	go func() {
		defer wg.Done()
		// A runtime that returns on its own takes the listener with it: a
		// process accepting webhooks it is no longer ingesting is a delivery
		// the source believes it has made.
		defer stop()
		fail("runtime", runtime.Run(runtimeCtx))
	}()
	wg.Wait()
	log.InfoContext(ctx, "connectors stopped")
	return errors.Join(errs...)
}

// stores is what events are written to and where backfill positions are kept:
// the L0 store over the process's pool, or whatever the caller supplied
// instead.
func stores(deps Deps) (connector.Sink, connector.CursorStore, error) {
	sink, cursors := deps.Sink, deps.Cursors
	if (sink == nil || cursors == nil) && deps.Pool == nil {
		return nil, nil, errors.New("the connectors service needs a database to write events to: pass --database-url or set HEARSAY_DATABASE_URL")
	}
	if sink == nil {
		sink = l0.New(deps.Pool)
	}
	if cursors == nil {
		cursors = l0.NewBackfillCursors(deps.Pool)
	}
	return sink, cursors, nil
}

// listen opens the service's listener.
func listen(ctx context.Context, deps Deps) (net.Listener, error) {
	if deps.Listener != nil {
		return deps.Listener, nil
	}
	addr := deps.Listen
	if addr == "" {
		addr = DefaultListen
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	return listener, nil
}

// serve runs the HTTP surface until ctx is cancelled, then shuts it down. A
// server that was asked to close is not a server that failed.
func serve(ctx context.Context, server *http.Server, listener net.Listener) error {
	done := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	// The requests in flight get a moment to finish, on a context of their own:
	// the one that stopped the process is already done.
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(grace); err != nil {
		return fmt.Errorf("shutting the HTTP server down: %w", err)
	}
	return <-done
}

// Select narrows the configured sources to the ones this process was asked to
// host. An empty selection is all of them.
//
// A name that is not a configured source is an error: a process started with
// `--source githbu` would otherwise host nothing at all and say so only in a
// count nobody reads.
func Select(configured []connector.SourceConfig, selected []string) ([]connector.SourceConfig, error) {
	if len(selected) == 0 {
		return configured, nil
	}
	byID := make(map[string]connector.SourceConfig, len(configured))
	for _, src := range configured {
		byID[src.ID] = src
	}
	out := make([]connector.SourceConfig, 0, len(selected))
	var unknown []string
	for _, name := range selected {
		src, ok := byID[name]
		switch {
		case !ok:
			unknown = append(unknown, name)
		case slices.ContainsFunc(out, func(s connector.SourceConfig) bool { return s.ID == name }):
			// Naming a source twice is not an error, but hosting it twice would
			// be: two connectors for one source share a cursor and a hook path.
		default:
			out = append(out, src)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("--source names %v, which the configuration does not: it has %v", unknown, ids(configured))
	}
	return out, nil
}

func ids(sources []connector.SourceConfig) []string {
	out := make([]string, 0, len(sources))
	for _, src := range sources {
		out = append(out, src.ID)
	}
	return out
}

// Selection renders which connectors a process is hosting for the `hosting` log
// field: the names it was given, or "all" when it was given none. Every line
// this service logs carries it, because "which connectors is this container
// running" is the first question asked of a process hosting a subset.
//
// It is not the `source` field. That one is ADR-0008's, is the source a line is
// about, and is what the runtime puts on the lines of each connector it drives;
// one process hosts several of them, so the two facts are two fields.
func Selection(sources []string) string {
	if len(sources) == 0 {
		return "all"
	}
	return strings.Join(sources, ",")
}

// handler is the service's HTTP surface: the runtime's own paths, and the two
// endpoints ADR-0008 gives every service.
func handler(runtime *connector.Runtime, pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	// Everything under the prefix is the runtime's, including the 404 for a
	// source that does not push.
	mux.Handle(connector.HookPrefix, runtime.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, readiness(r.Context(), runtime, pool))
	})
	return mux
}

// Readiness is what `/readyz` reports: whether this process is ingesting, and
// what each source it hosts says about itself (ADR-0008).
//
// It is served more widely than L0, so it carries ids and statuses and nothing
// else: no credential, no event text, no error from a source or from the
// database. What went wrong is in the log, which is not.
type Readiness struct {
	Status connector.HealthStatus `json:"status"`
	// Detail says what is wrong in the runtime's own words, which are a fixed
	// set of sentences rather than anything a source or Postgres said.
	Detail  string                   `json:"detail,omitempty"`
	Sources []connector.SourceHealth `json:"sources"`
}

func readiness(ctx context.Context, runtime *connector.Runtime, pool *pgxpool.Pool) Readiness {
	health := runtime.Health(ctx)
	out := Readiness{Status: health.Status, Sources: health.Sources}
	if pool == nil {
		return out
	}
	// ADR-0008 defines readiness as the database being reachable and its schema
	// acceptable per ADR-0006, and the credentials being present. The schema
	// half is what turns ADR-0006's version check into a deployment that stops
	// rather than one that half-works, so it is one query and not a ping. The
	// credentials half has nothing to check here: a source's secrets are
	// resolved from the environment at startup and a missing one refuses to
	// start the process (docs/connector-contract.md).
	//
	// Either failure is the operator's to read in the log, where the error can
	// name a host, a user and a version. The body says which of the two it is
	// and nothing else.
	log := telemetry.Logger(ctx)
	switch err := db.CheckSchema(ctx, pool); {
	case err == nil:
	case errors.Is(err, db.ErrSchemaBehind):
		log.ErrorContext(ctx, "readiness: the database schema is behind this binary", "error", err)
		out.Status = connector.HealthFailed
		out.Detail = "the database schema is behind this binary: run `hearsay migrate up`"
	default:
		log.ErrorContext(ctx, "readiness: the database cannot be reached", "error", err)
		out.Status = connector.HealthFailed
		out.Detail = "the database is unreachable"
	}
	return out
}

func writeJSON(w http.ResponseWriter, body Readiness) {
	status := http.StatusOK
	if body.Status == connector.HealthFailed {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// The response has already begun, so a failure here is a client that went
	// away rather than something this process can report.
	_ = enc.Encode(body)
}

// stoppingEarly turns a startup failure that happened because the process was
// asked to stop into a clean stop, the way the subcommands do: a service
// interrupted while it is still binding a port has not failed.
func stoppingEarly(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}
