//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

func TestAllMigratesTheDatabaseItIsPointedAt(t *testing.T) {
	if os.Getenv("HEARSAY_DATABASE_URL") == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"all", "--listen", "127.0.0.1:0", "--api-listen", "127.0.0.1:0", "--distiller-listen", "127.0.0.1:0", "--assert-worker-listen", "127.0.0.1:0", "--log-format", "json"}, io.Discard, stderr)
	}()

	started := func(out string) bool {
		return strings.Contains(out, "schema is current") && strings.Contains(out, "connectors started") &&
			strings.Contains(out, "api started") && strings.Contains(out, `"service":"distiller"`) && strings.Contains(out, `"service":"assert-worker"`) && strings.Count(out, "health probes started") >= 2
	}
	if !stderr.waitFor(10*time.Second, started) {
		cancel()
		t.Fatalf("`hearsay all` never reported the schema and its services:\n%s", stderr.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run(all) = %v, want nil", err)
	}
	if !strings.Contains(stderr.String(), `"schema_version":`) {
		t.Errorf("the schema line carries no version:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"listen":"127.0.0.1:`) {
		t.Errorf("`hearsay all` did not serve the connectors on the address --listen names:\n%s", stderr.String())
	}
	for line := range strings.Lines(stderr.String()) {
		if strings.Contains(line, `"msg":"api started"`) && !strings.Contains(line, `"listen":"127.0.0.1:`) {
			t.Errorf("`hearsay all` did not serve the API on the address --api-listen names: %s", line)
		}
	}
}

func TestConnectorsRunsAndStops(t *testing.T) {
	if os.Getenv("HEARSAY_DATABASE_URL") == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"connectors", "--listen", "127.0.0.1:0", "--log-format", "json"}, io.Discard, stderr)
	}()

	if !stderr.waitFor(10*time.Second, func(out string) bool { return strings.Contains(out, "connectors started") }) {
		cancel()
		t.Fatalf("`hearsay connectors` never started:\n%s", stderr.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run(connectors) = %v, want nil", err)
	}
	if !strings.Contains(stderr.String(), `"hosting":"all"`) {
		t.Errorf("the connectors service logged no hosting field:\n%s", stderr.String())
	}
}

func TestConnectorsRefusesASourceNobodyConfigured(t *testing.T) {
	if os.Getenv("HEARSAY_DATABASE_URL") == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"connectors", "--source", "githbu", "--listen", "127.0.0.1:0"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "githbu") {
		t.Fatalf("run(connectors --source githbu) = %v, want an error naming the source", err)
	}
}

func TestL0RefusesADatabaseBehindTheBinary(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	t.Setenv("HEARSAY_DATABASE_URL", swapDatabaseName(url, "postgres"))

	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"l0", "count"}, &stdout, &stderr)
	if !errors.Is(err, db.ErrSchemaBehind) {
		t.Fatalf("run(l0 count) against an unmigrated database = %v, want db.ErrSchemaBehind", err)
	}
	if !strings.Contains(err.Error(), "migrate up") {
		t.Errorf("the error does not say what to run: %v", err)
	}
}

func swapDatabaseName(url, name string) string {
	base, query, hasQuery := strings.Cut(url, "?")
	swapped := base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		return swapped + "?" + query
	}
	return swapped
}

type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	written chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written != nil {
		close(b.written)
		b.written = nil
	}
	return b.buf.Write(p)
}

func (b *syncBuffer) waitFor(within time.Duration, holds func(string) bool) bool {
	timeout := time.NewTimer(within)
	defer timeout.Stop()
	for {
		b.mu.Lock()
		if holds(b.buf.String()) {
			b.mu.Unlock()
			return true
		}
		if b.written == nil {
			b.written = make(chan struct{})
		}
		written := b.written
		b.mu.Unlock()
		select {
		case <-written:
		case <-timeout.C:
			return false
		}
	}
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestL0AndMigrateAgainstARealDatabase(t *testing.T) {
	if os.Getenv("HEARSAY_DATABASE_URL") == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	source := "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "cli"
	fake := connector.NewFake(connector.SourceConfig{ID: source, Type: connector.FakeType})
	event := fake.NewEvent(connector.KindMessage, "m1", "hello from the cli")

	pool, err := db.Connect(t.Context(), os.Getenv("HEARSAY_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	defer pool.Close()
	appended, err := l0.New(pool).Append(t.Context(), event)
	if err != nil {
		t.Fatalf("Append() = %v, want no error", err)
	}

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "migrate status lists the migrations",
			args: []string{"migrate", "status"},
			want: []string{"VERSION", "applied", "create_vector_extension", "create_l0_events"},
		},
		{
			name: "migrate up on a current database applies nothing and says where it is",
			args: []string{"migrate", "up"},
			want: []string{"schema version "},
		},
		{
			name: "l0 count reports the source",
			args: []string{"l0", "count"},
			want: []string{"SOURCE", source, "message"},
		},
		{
			name: "l0 list finds the event without printing its text",
			args: []string{"l0", "list", "--source", source},
			want: []string{appended.ID, "m1", "1 event(s)"},
		},
		{
			name: "l0 get prints the event",
			args: []string{"l0", "get", appended.ID},
			want: []string{appended.ID, "hello from the cli"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(t.Context(), tt.args, &stdout, &stderr); err != nil {
				t.Fatalf("run(%q) = %v, want nil (stderr: %s)", tt.args, err, stderr.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("run(%q) stdout does not contain %q:\n%s", tt.args, want, stdout.String())
				}
			}
		})
	}

	t.Run("l0 list prints no payload", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if err := run(t.Context(), []string{"l0", "list", "--source", source}, &stdout, &stderr); err != nil {
			t.Fatalf("run(l0 list) = %v, want nil", err)
		}
		if strings.Contains(stdout.String(), "hello from the cli") {
			t.Errorf("l0 list printed the payload text:\n%s", stdout.String())
		}
	})

	t.Run("l0 tail follows one source and returns when the context is cancelled", func(t *testing.T) {
		other := connector.NewFake(connector.SourceConfig{ID: source + "b", Type: connector.FakeType})
		if _, err := l0.New(pool).Append(t.Context(), other.NewEvent(connector.KindMessage, "m1", "another source")); err != nil {
			t.Fatalf("Append(another source) = %v, want no error", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var stdout, stderr syncBuffer
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, []string{"l0", "tail", "--source", source, "--interval", "50ms"}, &stdout, &stderr)
		}()
		waitFor := func(id string) bool {
			return stdout.waitFor(10*time.Second, func(out string) bool { return strings.Contains(out, id) })
		}
		if !waitFor(appended.ID) {
			t.Errorf("l0 tail did not print the event within 10s:\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
		} else {
			if _, err := l0.New(pool).Append(t.Context(), other.NewEvent(connector.KindMessage, "m2", "another source, later")); err != nil {
				t.Fatalf("Append(another source) = %v, want no error", err)
			}
			later, err := l0.New(pool).Append(t.Context(), fake.NewEvent(connector.KindMessage, "m2", "followed"))
			if err != nil {
				t.Fatalf("Append(followed) = %v, want no error", err)
			}
			if !waitFor(later.ID) {
				t.Errorf("l0 tail did not follow an event appended while it ran:\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run(l0 tail) = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("run(l0 tail) did not return within 5s of cancellation")
		}
		if !strings.Contains(stdout.String(), appended.ID) {
			t.Errorf("l0 tail did not print the event:\n%s", stdout.String())
		}
		if strings.Contains(stdout.String(), source+"b") {
			t.Errorf("l0 tail --source printed another source's events:\n%s", stdout.String())
		}
	})
}
