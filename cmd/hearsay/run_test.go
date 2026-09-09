package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// The four service subcommand names are a contract (ADR-0003): the Dagger
// module, the compose file and the deployment manifests all name them.
func TestServiceSubcommandNamesAreTheOnesTheADRFixes(t *testing.T) {
	want := []string{"connectors", "distiller", "assert-worker", "api", "migrate", "version", "all"}
	have := map[string]bool{}
	for _, cmd := range commands() {
		have[cmd.name] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("subcommand %q is missing; commands are %v", name, have)
		}
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    string // substring; empty means no error
		wantStdout string // substring
		wantStderr string // substring
	}{
		{
			name:       "no arguments prints usage and fails",
			args:       nil,
			wantErr:    "no subcommand",
			wantStderr: "Usage:",
		},
		{
			name:       "unknown subcommand names it",
			args:       []string{"distill"},
			wantErr:    `unknown subcommand "distill"`,
			wantStderr: "Usage:",
		},
		{
			name:       "help prints usage on stdout",
			args:       []string{"help"},
			wantStdout: "Subcommands:",
		},
		{
			name:       "--help prints usage on stdout",
			args:       []string{"--help"},
			wantStdout: "assert-worker",
		},
		{
			name:       "-h prints usage on stdout",
			args:       []string{"-h"},
			wantStdout: "connectors",
		},
		{
			name:       "version prints the build identity",
			args:       []string{"version"},
			wantStdout: "hearsay ",
		},
		{
			name:       "--version is version",
			args:       []string{"--version"},
			wantStdout: "hearsay ",
		},
		{
			name:    "migrate is honest about not being built",
			args:    []string{"migrate", "up"},
			wantErr: "not implemented yet",
		},
		{
			name:    "migrate rejects an unknown action",
			args:    []string{"migrate", "sideways"},
			wantErr: `unknown action "sideways"`,
		},
		{
			name:    "a service rejects an unknown flag",
			args:    []string{"api", "--verbose"},
			wantErr: "flag provided but not defined",
		},
		{
			name:    "a service rejects a stray argument",
			args:    []string{"api", "start"},
			wantErr: `unexpected argument "start"`,
		},
		{
			name:    "a bad log level is caught before the service starts",
			args:    []string{"api", "--log-level", "chatty"},
			wantErr: "unknown log level",
		},
		{
			name:    "an empty --source is rejected",
			args:    []string{"connectors", "--source", ""},
			wantErr: "source name is empty",
		},
		{
			name:       "a subcommand's --help is not an error",
			args:       []string{"connectors", "--help"},
			wantStderr: "-source",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// Cancelled up front: the service subcommands block until their
			// context is done, and these cases must not depend on that.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			err := run(ctx, tt.args, &stdout, &stderr)

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("run(%q) = %v, want no error", tt.args, err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("run(%q) = nil, want an error containing %q", tt.args, tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("run(%q) = %v, want an error containing %q", tt.args, err, tt.wantErr)
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// Every long-running subcommand has to come back when the process is signalled,
// which is what main's signal context does to it. Each one also has to say on
// its log lines which service it is — once, not once per wrapper it passed
// through.
func TestServiceSubcommandsReturnWhenTheContextIsCancelled(t *testing.T) {
	tests := []struct {
		args     []string
		services []string // the values the service field must take
	}{
		{args: []string{"connectors"}, services: []string{"connectors"}},
		{args: []string{"connectors", "--source", "github"}, services: []string{"connectors"}},
		{args: []string{"distiller"}, services: []string{"distiller"}},
		{args: []string{"assert-worker"}, services: []string{"assert-worker"}},
		{args: []string{"api"}, services: []string{"api"}},
		{args: []string{"all"}, services: []string{"connectors", "distiller", "assert-worker", "api"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append(slices.Clone(tt.args), "--log-format", "json")
			ctx, cancel := context.WithCancel(t.Context())

			done := make(chan error, 1)
			go func() { done <- run(ctx, args, &stdout, &stderr) }()
			cancel()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("run(%q) = %v, want nil", args, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("run(%q) did not return within 5s of cancellation", args)
			}

			seen := map[string]bool{}
			for line := range strings.Lines(strings.TrimSpace(stderr.String())) {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				// json.Unmarshal keeps the last of a repeated key, so count the
				// occurrences rather than the decoded value.
				if n := strings.Count(line, `"service":`); n != 1 {
					t.Errorf("log line has the service field %d times, want once: %s", n, line)
				}
				var rec struct {
					Service string `json:"service"`
				}
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("log line is not JSON: %v (%s)", err, line)
				}
				seen[rec.Service] = true
			}
			for _, want := range tt.services {
				if !seen[want] {
					t.Errorf("no log line from service %q; saw %v", want, slices.Sorted(maps.Keys(seen)))
				}
			}
		})
	}
}

func TestLogFlagsAndEnvironment(t *testing.T) {
	t.Run("HEARSAY_LOG_FORMAT is honoured", func(t *testing.T) {
		t.Setenv("HEARSAY_LOG_FORMAT", "text")
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := run(ctx, []string{"api"}, &stdout, &stderr); err != nil {
			t.Fatalf("run(api) = %v, want nil", err)
		}
		if !strings.Contains(stderr.String(), "service=api") {
			t.Errorf("stderr = %q, want text-format output", stderr.String())
		}
	})

	t.Run("the flag beats the environment", func(t *testing.T) {
		t.Setenv("HEARSAY_LOG_FORMAT", "text")
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := run(ctx, []string{"api", "--log-format", "json"}, &stdout, &stderr); err != nil {
			t.Fatalf("run(api) = %v, want nil", err)
		}
		if !strings.Contains(stderr.String(), `"service":"api"`) {
			t.Errorf("stderr = %q, want JSON-format output", stderr.String())
		}
	})

	t.Run("HEARSAY_LOG_LEVEL is honoured", func(t *testing.T) {
		t.Setenv("HEARSAY_LOG_LEVEL", "error")
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := run(ctx, []string{"api"}, &stdout, &stderr); err != nil {
			t.Fatalf("run(api) = %v, want nil", err)
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want nothing below error level", stderr.String())
		}
	})
}

func TestMigrateErrorIsNotImplemented(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"migrate", "status"}, &stdout, &stderr)
	if !errors.Is(err, errNotImplemented) {
		t.Fatalf("run(migrate status) = %v, want it to carry errNotImplemented", err)
	}
	if !strings.Contains(err.Error(), "adr") && !strings.Contains(err.Error(), "ADR") {
		t.Errorf("error %q does not point at the ADR", err)
	}
}
