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

// `hearsay all` is local development only, and ADR-0006 has it migrate for
// itself because a local database is nobody's production. Every other command
// verifies and refuses.
func TestAllMigratesTheDatabaseItIsPointedAt(t *testing.T) {
	if os.Getenv("HEARSAY_DATABASE_URL") == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"all", "--log-format", "json"}, io.Discard, stderr)
	}()

	// Cancel once the schema line has gone out, so that the migration is never
	// racing the shutdown.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "schema is current") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("`hearsay all` never reported the schema:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run(all) = %v, want nil", err)
	}
	if !strings.Contains(stderr.String(), `"schema_version":`) {
		t.Errorf("the schema line carries no version:\n%s", stderr.String())
	}
}

// The connectors service against a real database: the wiring from the flags to
// the pool, the runtime and its listener, and the `source` field every line of
// this service carries.
func TestConnectorsRunsAndStops(t *testing.T) {
	if os.Getenv("HEARSAY_DATABASE_URL") == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		// Port 0: a test binds what the operating system gives it, not the
		// port a deployment uses.
		done <- run(ctx, []string{"connectors", "--listen", "127.0.0.1:0", "--log-format", "json"}, io.Discard, stderr)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "connectors started") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("`hearsay connectors` never started:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run(connectors) = %v, want nil", err)
	}
	// Which connectors a process is hosting is the first question asked of one,
	// so every line it logs says. It is `hosting` and not `source`: ADR-0008's
	// `source` is the source a line is about, and the runtime writes that.
	if !strings.Contains(stderr.String(), `"hosting":"all"`) {
		t.Errorf("the connectors service logged no hosting field:\n%s", stderr.String())
	}
}

// `--source` names a configured source. A name nothing is configured under is a
// startup failure rather than a process that hosts nothing.
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

// A database with no Hearsay schema in it is one every command but `migrate`
// and `all` refuses, saying what to run (ADR-0006).
func TestL0RefusesADatabaseBehindTheBinary(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	// The maintenance database every Postgres server has, and nothing migrates.
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

// swapDatabaseName points a connection URL at another database on the same
// server.
func swapDatabaseName(url, name string) string {
	base, query, hasQuery := strings.Cut(url, "?")
	swapped := base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		return swapped + "?" + query
	}
	return swapped
}

// syncBuffer is a bytes.Buffer a test can read while a goroutine writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The subcommands that talk to Postgres, end to end: the wiring between a flag,
// a pool and a store is not covered by anything else.
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
		want []string // substrings stdout must contain
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

	// A listing is read over someone's shoulder, so it carries no payload text.
	t.Run("l0 list prints no payload", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if err := run(t.Context(), []string{"l0", "list", "--source", source}, &stdout, &stderr); err != nil {
			t.Fatalf("run(l0 list) = %v, want nil", err)
		}
		if strings.Contains(stdout.String(), "hello from the cli") {
			t.Errorf("l0 list printed the payload text:\n%s", stdout.String())
		}
	})

	// tail follows until the process is interrupted, and ending that way is not
	// a failure. --source narrows what it follows: an operator watching one
	// noisy source has to get that source and not the store.
	t.Run("l0 tail follows one source and returns when the context is cancelled", func(t *testing.T) {
		other := connector.NewFake(connector.SourceConfig{ID: source + "b", Type: connector.FakeType})
		if _, err := l0.New(pool).Append(t.Context(), other.NewEvent(connector.KindMessage, "m1", "another source")); err != nil {
			t.Fatalf("Append(another source) = %v, want no error", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		var stdout, stderr bytes.Buffer
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, []string{"l0", "tail", "--source", source, "--interval", "50ms"}, &stdout, &stderr)
		}()
		time.Sleep(200 * time.Millisecond)
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
