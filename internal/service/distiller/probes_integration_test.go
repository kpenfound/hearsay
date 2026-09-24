//go:build integration

package distiller_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestProbes(t *testing.T) {
	pool, err := db.Connect(t.Context(), databaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- distiller.Run(ctx, &config.Config{}, distiller.Deps{Pool: pool, Listener: listener}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	get := func(path string) (int, string) {
		t.Helper()
		var resp *http.Response
		var err error
		for i := 0; i < 100; i++ {
			resp, err = client.Get("http://" + listener.Addr().String() + path)
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	if code, body := get("/healthz"); code != 200 || body != "ok\n" {
		t.Errorf("health = %d %q", code, body)
	}
	if code, body := get("/readyz"); code != 200 || body != `{"status":"ok"}` {
		t.Errorf("ready = %d %q", code, body)
	}
	pool.Close()
	if code, body := get("/readyz"); code != 503 || body != `{"status":"failed","detail":"the database is unreachable"}` {
		t.Errorf("failed ready = %d %q", code, body)
	}
	if code, body := get("/healthz"); code != 200 || body != "ok\n" {
		t.Errorf("health after database loss = %d %q", code, body)
	}
}

func databaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	return url
}
