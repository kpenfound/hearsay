// Package servicetest holds the test support the worker services share.
package servicetest

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Probes starts a service through run on a loopback listener and checks its
// health and readiness endpoints, before and after the database is lost.
func Probes(t *testing.T, pool *pgxpool.Pool, run func(ctx context.Context, listener net.Listener) error) {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, listener) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	get := func(path string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	if code, body := get("/healthz"); code != 200 || body != "ok\n" {
		t.Errorf("health = %d %q", code, body)
	}
	if code, body := get("/readyz"); code != 200 || body != `{"status":"ok","judgments":{"healthy":true,"observations":0,"attempts":0,"failures":0}}` {
		t.Errorf("ready = %d %q", code, body)
	}
	pool.Close()
	if code, body := get("/readyz"); code != 503 || body != `{"status":"failed","detail":"the database is unreachable","judgments":{"healthy":true,"observations":0,"attempts":0,"failures":0}}` {
		t.Errorf("failed ready = %d %q", code, body)
	}
	if code, body := get("/healthz"); code != 200 || body != "ok\n" {
		t.Errorf("health after database loss = %d %q", code, body)
	}
}
