// Package connectors hosts the source connectors. Connectors write L0 and
// nothing else: they turn a Slack thread, a GitHub webhook or a Drive change
// into events, and every layer above is someone else's job. The one exception
// is the process's, not a connector's: a chat command a connector receives is
// applied here, through the shared applier ([l2.Commands]), so the
// connector can answer it.
//
// One process can host several connectors; which ones it hosts is the
// `--source` selection. What it hosts them with is [connector.Runtime]: this
// package is the wiring — the configured sources, the registry of factories,
// the L0 store to write to, the cursor store to keep backfill positions in, the
// store ACL re-syncs are kept in and the L0 read that finds a container still
// served as public — and the HTTP surface the runtime needs, which is the push
// connectors' handlers and the service's health.
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
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Name is the subcommand this service runs as.
const Name = "connectors"

// DefaultListen is where the service serves the push connectors' handlers and
// its own health when nothing says otherwise.
const DefaultListen = ":8081"

const shutdownGrace = 5 * time.Second

// Deps are the dependencies the process builds and hands to [Run].
type Deps struct {
	// Sources selects which configured connectors to host. Empty means all of
	// them. A name that is not a configured source is a startup failure.
	Sources []string
	// Pool is the database. The caller owns it and closes it. It is
	// what [Sink] and [Deps.Cursors] are built from when they are not supplied.
	Pool *pgxpool.Pool
	// Registry is the connector types this binary can ingest.
	Registry *connector.Registry
	// Sink is where events land. Nil is the L0 store over [Deps.Pool].
	Sink connector.Sink
	// Cursors is where backfill positions are kept. Nil is the backfill cursor
	// store over [Deps.Pool].
	Cursors connector.CursorStore
	// Resyncs is where ACL re-syncs are kept, and Exposure is what says which
	// containers L0 serves as public. Nil is the store over [Deps.Pool].
	Resyncs  connector.ResyncStore
	Exposure connector.ExposureReader
	// Commands is what the chat commands a connector receives are applied
	// through. Nil is [l2.Commands] over [Deps.Pool] and the configuration;
	// with no pool either, commands are refused unrecorded.
	Commands connector.CommandApplier
	// Listener is what the HTTP surface is served on. Nil is a listener on
	// [Deps.Listen].
	Listener net.Listener
	// Listen is the address to serve on when [Deps.Listener] is nil. Empty is
	// [DefaultListen].
	Listen string
	// Cadence tunes the runtime's own timings. The zero value is the defaults.
	Cadence connector.Cadence
}

// Run hosts the configured connectors until ctx is cancelled, and returns nil
// when it stops that way.
//
// The runtime and the HTTP surface stop together and in that order.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
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
	resyncs, exposure := resyncStores(deps)
	commands := deps.Commands
	if commands == nil && deps.Pool != nil {
		c, err := l2.NewCommands(deps.Pool, cfg.Repo)
		if err != nil {
			return errors.Join(err, listener.Close())
		}
		commands = c
	}
	runtime, err := connector.NewRuntime(ctx, connector.RuntimeOptions{
		Sources:  sources,
		Registry: registry,
		Sink:     sink,
		Cursors:  cursors,
		Resyncs:  resyncs,
		Exposure: exposure,
		Commands: commands,
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

		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}

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

		defer stopRuntime()
		fail("http", serve(ctx, server, listener))
	}()
	go func() {
		defer wg.Done()

		defer stop()
		fail("runtime", runtime.Run(runtimeCtx))
	}()
	wg.Wait()
	log.InfoContext(ctx, "connectors stopped")
	return errors.Join(errs...)
}

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

func resyncStores(deps Deps) (connector.ResyncStore, connector.ExposureReader) {
	resyncs, exposure := deps.Resyncs, deps.Exposure
	if deps.Pool != nil {
		if resyncs == nil {
			resyncs = l0.NewResyncs(deps.Pool)
		}
		if exposure == nil {
			exposure = l0.New(deps.Pool)
		}
	}
	return resyncs, exposure
}

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
// A name that is not a configured source is an error.
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
// this service logs carries it. It is not the `source` field, which is the
// source a line is about.
func Selection(sources []string) string {
	if len(sources) == 0 {
		return "all"
	}
	return strings.Join(sources, ",")
}

func handler(runtime *connector.Runtime, pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()

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
// what each source it hosts says about itself.
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

	_ = enc.Encode(body)
}

func stoppingEarly(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}
