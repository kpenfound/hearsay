package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// RegisterProbes adds the common liveness and database/schema readiness paths.
func RegisterProbes(mux *http.ServeMux, pool *pgxpool.Pool) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		status, detail := http.StatusOK, ""
		if pool != nil {
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
}

// Probes is the common HTTP surface of the distiller and assertion worker.
func Probes(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	RegisterProbes(mux, pool)
	return mux
}

// RunProbes serves until cancellation, with a short graceful shutdown.
func RunProbes(ctx context.Context, name, addr string, listener net.Listener, pool *pgxpool.Pool) error {
	if listener == nil {
		var lc net.ListenConfig
		var err error
		listener, err = lc.Listen(ctx, "tcp", addr)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("listening on %s: %w", addr, err)
		}
	}
	server := &http.Server{
		Handler:           Probes(pool),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	telemetry.Logger(ctx).InfoContext(ctx, "health probes started", "service", name, "listen", listener.Addr().String())
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
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return errors.Join(server.Shutdown(grace), <-done)
	}
}
