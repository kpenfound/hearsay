// Package api serves the read API over MCP and HTTP: bundle assembly, the
// handles a consumer follows, and the audit trail. Reads are structured
// lookups, and nothing here asks a model to generate anything: the one model
// call a read makes is `search` embedding the query it was given, on the
// `embed` tier (internal/l1).
//
// Both interfaces are thin over one call layer, [Calls]: a call takes the
// caller, a name and JSON arguments, and returns the bytes that are served. HTTP
// serves them as the response body of `POST /v1/<call>`; MCP serves them as the
// text of a tools/call result at `/mcp`. There is one encoding and it is not the
// interface's, which is what makes the same request byte-identical over both.
//
// Both interfaces authenticate the person named in [PrincipalHeader] with a
// bearer token. When [AgentHeader] names an agent, that agent's token is also
// required. Authentication precedes the shared call layer (ADR-0014).
//
// The assert and watch calls of docs/design.md#read-and-assert-api are later
// work, and so are a bundle's directive, anchors and conflicts.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "api"

// DefaultListen is where the API serves when nothing says otherwise.
const DefaultListen = ":8080"

// MaxRequest is the largest request body the API reads. A call's arguments are
// a scope, an id or a query: kilobytes, not megabytes.
const MaxRequest = 1 << 20

// shutdownGrace is how long requests in flight get once the process is asked
// to stop.
const shutdownGrace = 5 * time.Second

// Deps are the dependencies the process builds and hands to Run.
type Deps struct {
	// Pool is the database (ADR-0004). The caller owns it and closes it.
	Pool *pgxpool.Pool
	// LLM is the model tier registry; the API uses the `embed` tier alone, for
	// search, and runs full-text search without it. Nil is a process given no
	// configuration.
	LLM llm.Registry
	// Listener is what the API is served on. Nil is a listener on Listen; a
	// test passes one bound to port 0.
	Listener net.Listener
	// Listen is the address to serve on when Listener is nil. Empty is
	// [DefaultListen].
	Listen string
}

// Run serves the API until ctx is cancelled, and returns nil when it stops that
// way.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	if deps.Pool == nil {
		return errors.New("the API needs a database: pass --database-url or set HEARSAY_DATABASE_URL")
	}
	log := telemetry.Logger(ctx)
	embedder, err := embedTier(deps.LLM)
	if err != nil {
		return err
	}
	calls, err := NewCalls(deps.Pool, cfg.Repo, embedder)
	if err != nil {
		return err
	}
	listener := deps.Listener
	if listener == nil {
		addr := deps.Listen
		if addr == "" {
			addr = DefaultListen
		}
		var lc net.ListenConfig
		if listener, err = lc.Listen(ctx, "tcp", addr); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("listening on %s: %w", addr, err)
		}
	}
	server := &http.Server{
		Handler:           Handler(calls, deps.Pool),
		ReadHeaderTimeout: 10 * time.Second,
		// Not the cancellable context: a request in flight keeps its logger
		// through the shutdown grace period.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	log.InfoContext(ctx, "api started", "listen", listener.Addr().String(), "config_digest", cfg.Repo.Digest,
		"embedding", embedder != nil)

	var wg sync.WaitGroup
	done := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		err = server.Shutdown(grace)
		cancel()
		err = errors.Join(err, <-done)
	}
	wg.Wait()
	log.InfoContext(ctx, "api stopped")
	return err
}

// embedTier is the `embed` tier, or nil where there is no registry or it names
// none: search then runs on full text alone. A tier of the wrong width stops
// the process, as it does the distiller's.
func embedTier(registry llm.Registry) (l1.Embedder, error) {
	if registry == nil {
		return nil, nil
	}
	embedder, err := registry.Embedder()
	if errors.Is(err, llm.ErrTierNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := llm.CheckDimensions(embedder, l1.EmbeddingDimensions); err != nil {
		return nil, err
	}
	return embedder, nil
}

// Handler is the API's HTTP surface: the calls over HTTP and over MCP, and the
// two endpoints ADR-0008 gives every service. A nil pool serves readiness
// without the database check, for a test.
func Handler(calls *Calls, pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/{call}", func(w http.ResponseWriter, r *http.Request) {
		caller, authErr := calls.auth.authenticate(r.Header)
		if authErr != nil {
			writeError(w, authErr)
			return
		}
		args, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequest))
		if err != nil {
			writeError(w, fail(http.StatusRequestEntityTooLarge, "the request body is over %d bytes", MaxRequest))
			return
		}
		body, err := calls.Call(r.Context(), caller, r.PathValue("call"), args)
		if err != nil {
			writeError(w, asError(err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET /v1/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Trim(r.URL.Path, "/") != "v1" {
			writeError(w, fail(http.StatusMethodNotAllowed, "a call is a POST"))
			return
		}
		body, _ := json.Marshal(struct {
			Tools []Tool `json:"tools"`
		}{Tools()})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.Handle("/mcp", MCP(calls))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		status, detail := http.StatusOK, ""
		if pool != nil {
			// The database reachable and its schema acceptable, as one query
			// (ADR-0006, ADR-0008). The body carries a fixed sentence; the
			// error is in the log.
			switch err := db.CheckSchema(r.Context(), pool); {
			case err == nil:
			case errors.Is(err, db.ErrSchemaBehind):
				telemetry.Logger(r.Context()).ErrorContext(r.Context(), "readiness: the database schema is behind this binary", "error", err)
				status, detail = http.StatusServiceUnavailable, "the database schema is behind this binary: run `hearsay migrate up`"
			default:
				telemetry.Logger(r.Context()).ErrorContext(r.Context(), "readiness: the database cannot be reached", "error", err)
				status, detail = http.StatusServiceUnavailable, "the database is unreachable"
			}
		}
		body, _ := json.Marshal(struct {
			Status string `json:"status"`
			Detail string `json:"detail,omitempty"`
		}{map[bool]string{true: "ok", false: "failed"}[status == http.StatusOK], detail})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
	return mux
}

func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return errInternal
}

func writeError(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(ErrorBody(e))
}
