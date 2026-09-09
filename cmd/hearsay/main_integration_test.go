//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

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
	// a failure.
	t.Run("l0 tail returns when the context is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var stdout, stderr bytes.Buffer
		done := make(chan error, 1)
		go func() { done <- run(ctx, []string{"l0", "tail", "--interval", "50ms"}, &stdout, &stderr) }()
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
	})
}
