package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
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
			name:    "version rejects a stray argument",
			args:    []string{"version", "extra"},
			wantErr: `unexpected argument "extra"`,
		},
		{
			name:    "version rejects a flag it does not have",
			args:    []string{"version", "--log-level", "bogus"},
			wantErr: "flag provided but not defined",
		},
		{
			name:    "help rejects a stray argument",
			args:    []string{"help", "connectors"},
			wantErr: `unexpected argument "connectors"`,
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
// which is what main's signal context does to it. Each one also has to carry the
// three fields ADR-0008 says a process attaches at startup — service, version,
// instance — on every line, once each rather than once per wrapper the line
// passed through.
func TestServiceSubcommandsReturnWhenTheContextIsCancelled(t *testing.T) {
	t.Setenv("HEARSAY_INSTANCE", "replica-7")
	tests := []struct {
		args     []string
		services []string // the values the service field must take
		source   string   // the value of the source field, where there is one
	}{
		{args: []string{"connectors"}, services: []string{"connectors"}, source: "all"},
		{args: []string{"connectors", "--source", "github"}, services: []string{"connectors"}, source: "github"},
		{args: []string{"connectors", "--source", "github", "--source", "slack"}, services: []string{"connectors"}, source: "github,slack"},
		{args: []string{"distiller"}, services: []string{"distiller"}},
		{args: []string{"assert-worker"}, services: []string{"assert-worker"}},
		{args: []string{"api"}, services: []string{"api"}},
		{args: []string{"all"}, services: []string{"connectors", "distiller", "assert-worker", "api"}, source: "all"},
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
			sources := map[string]bool{}
			for line := range strings.Lines(strings.TrimSpace(stderr.String())) {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				// json.Unmarshal keeps the last of a repeated key, so count the
				// occurrences rather than trusting the decoded value.
				for _, field := range []string{"service", "version", "instance"} {
					if n := strings.Count(line, `"`+field+`":`); n != 1 {
						t.Errorf("log line has the %s field %d times, want once: %s", field, n, line)
					}
				}
				var rec struct {
					Service  string `json:"service"`
					Version  string `json:"version"`
					Instance string `json:"instance"`
					Source   string `json:"source"`
				}
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("log line is not JSON: %v (%s)", err, line)
				}
				if rec.Version == "" {
					t.Errorf("log line has no version: %s", line)
				}
				if rec.Instance != "replica-7" {
					t.Errorf("instance = %q, want the value of HEARSAY_INSTANCE: %s", rec.Instance, line)
				}
				seen[rec.Service] = true
				if rec.Service == "connectors" {
					sources[rec.Source] = true
				}
			}
			for _, want := range tt.services {
				if !seen[want] {
					t.Errorf("no log line from service %q; saw %v", want, slices.Sorted(maps.Keys(seen)))
				}
			}
			// --source has to be visible in the log, or nothing distinguishes a
			// process hosting one connector from one hosting all of them.
			if tt.source != "" && !sources[tt.source] {
				t.Errorf("connectors logged source %v, want %q", slices.Sorted(maps.Keys(sources)), tt.source)
			}
		})
	}
}

func TestInstanceDefaultsToTheHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no hostname available: %v", err)
	}
	t.Setenv("HEARSAY_INSTANCE", "")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := run(ctx, []string{"api", "--log-format", "json"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if want := `"instance":"` + host + `"`; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %s", stderr.String(), want)
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
	// run already wraps with the subcommand name, so the handler must not.
	if n := strings.Count(err.Error(), "migrate"); n != 1 {
		t.Errorf("error names the subcommand %d times, want once: %v", n, err)
	}
}

// writeConfig writes a configuration repository into a temporary directory and
// returns its path.
func writeConfig(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", full, err)
		}
	}
	return root
}

// validConfig is the smallest configuration that loads.
var validConfig = map[string]string{
	"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
	"scopes/api.yaml":     "id: api\nsources: [github]\n",
}

func TestConfigValidate(t *testing.T) {
	valid := writeConfig(t, validConfig)
	broken := writeConfig(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":     "id: api\nsources: [github, discord]\n",
	})

	tests := []struct {
		name       string
		args       []string
		wantErr    string
		wantStdout string
	}{
		{
			name:       "a valid configuration is summarised",
			args:       []string{"config", "validate", valid},
			wantStdout: "is valid",
		},
		{
			name:       "the summary carries the digest a running process logs",
			args:       []string{"config", "validate", valid},
			wantStdout: "sha256:",
		},
		{
			name:       "the path may come from the flag instead",
			args:       []string{"config", "validate", "--config", valid},
			wantStdout: "is valid",
		},
		{
			name:    "an invalid configuration names the file, the line and the field",
			args:    []string{"config", "validate", broken},
			wantErr: `scopes/api.yaml:1: scope "api": sources[1].source: no source is configured with id "discord"`,
		},
		{
			name:    "config with no action says what it wanted",
			args:    []string{"config"},
			wantErr: "no action given: want validate",
		},
		{
			name:    "an unknown action is named",
			args:    []string{"config", "explain"},
			wantErr: `unknown action "explain"`,
		},
		{
			name:    "a stray argument is refused",
			args:    []string{"config", "validate", valid, "twice"},
			wantErr: `unexpected argument "twice"`,
		},
		{
			name:    "a path that is not there",
			args:    []string{"config", "validate", filepath.Join(valid, "nowhere")},
			wantErr: "reading configuration at",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), tt.args, &stdout, &stderr)
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
		})
	}
}

// A service reads its configuration before it starts, so a mistake in it stops
// the process rather than showing up on the first request (ADR-0009).
func TestServicesLoadTheirConfiguration(t *testing.T) {
	valid := writeConfig(t, validConfig)

	t.Run("a bad configuration stops the service starting", func(t *testing.T) {
		broken := writeConfig(t, map[string]string{"sources/github.yaml": "id: github\n"})
		var stdout, stderr bytes.Buffer
		err := run(t.Context(), []string{"api", "--config", broken}, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "not a valid configuration") {
			t.Fatalf("run(api --config <broken>) = %v, want the configuration error", err)
		}
	})

	t.Run("a loaded configuration is logged with its digest", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := run(ctx, []string{"api", "--config", valid, "--log-format", "json"}, &stdout, &stderr); err != nil {
			t.Fatalf("run(api --config <valid>) = %v, want no error", err)
		}
		for _, want := range []string{`"msg":"configuration loaded"`, `"config_digest":"sha256:`, `"sources":1`} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q, want it to contain %s", stderr.String(), want)
			}
		}
	})

	t.Run("HEARSAY_CONFIG is honoured", func(t *testing.T) {
		t.Setenv("HEARSAY_CONFIG", valid)
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := run(ctx, []string{"api", "--log-format", "json"}, &stdout, &stderr); err != nil {
			t.Fatalf("run(api) = %v, want no error", err)
		}
		if !strings.Contains(stderr.String(), `"msg":"configuration loaded"`) {
			t.Errorf("stderr = %q, want the configuration to have been loaded", stderr.String())
		}
	})

	t.Run("no configuration is a warning, not a refusal", func(t *testing.T) {
		t.Setenv("HEARSAY_CONFIG", "")
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := run(ctx, []string{"api", "--log-format", "json"}, &stdout, &stderr); err != nil {
			t.Fatalf("run(api) = %v, want no error", err)
		}
		if !strings.Contains(stderr.String(), `"level":"WARN"`) || !strings.Contains(stderr.String(), "no configuration") {
			t.Errorf("stderr = %q, want a warning that there is no configuration", stderr.String())
		}
	})
}
