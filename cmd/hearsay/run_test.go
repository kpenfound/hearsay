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

	"github.com/kpenfound/hearsay/internal/db"
)

// The four service subcommand names are a contract (ADR-0003): the Dagger
// module, the compose file and the deployment manifests all name them.
func TestServiceSubcommandNamesAreTheOnesTheADRFixes(t *testing.T) {
	want := []string{"connectors", "distiller", "assert-worker", "api", "migrate", "l0", "version", "all"}
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
			name:    "migrate down refuses without --i-know",
			args:    []string{"migrate", "down"},
			wantErr: "--i-know",
		},
		{
			name:    "migrate up-to needs a version",
			args:    []string{"migrate", "up-to"},
			wantErr: "one argument",
		},
		{
			name:    "migrate up-to rejects a version that is not a number",
			args:    []string{"migrate", "up-to", "latest"},
			wantErr: `"latest" is not a migration version`,
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
			name:    "l0 rejects an unknown action",
			args:    []string{"l0", "grep"},
			wantErr: `unknown action "grep"`,
		},
		{
			name:    "l0 needs an action",
			args:    []string{"l0"},
			wantErr: "no action given",
		},
		{
			name:    "l0 get needs an event id",
			args:    []string{"l0", "get"},
			wantErr: "one argument",
		},
		{
			name:    "l0 tail rejects an interval of zero",
			args:    []string{"l0", "tail", "--interval", "0s"},
			wantErr: "--interval must be positive",
		},
		{
			name:    "l0 tail rejects a cursor that is not one",
			args:    []string{"l0", "tail", "--cursor", "yesterday"},
			wantErr: `cursor "yesterday"`,
		},
		{
			name:    "l0 refuses an artifact without a source",
			args:    []string{"l0", "list", "--artifact", "acme/api#1"},
			wantErr: "needs a source",
		},
		{
			name:    "l0 tail refuses an artifact without a source too",
			args:    []string{"l0", "tail", "--artifact", "acme/api#1"},
			wantErr: "needs a source",
		},
		{
			name:    "l0 count does not filter, and says so",
			args:    []string{"l0", "count", "--source", "github-acme"},
			wantErr: "count does not read --source",
		},
		{
			name:    "l0 get takes no filter",
			args:    []string{"l0", "get", "--kind", "message", "evt:a:b"},
			wantErr: "get does not read --kind",
		},
		{
			name:    "l0 tail does not order",
			args:    []string{"l0", "tail", "--newest"},
			wantErr: "tail does not read --newest",
		},
		{
			name:    "l0 list does not tail",
			args:    []string{"l0", "list", "--interval", "1s", "--cursor", "1.2"},
			wantErr: "list does not read --cursor, --interval",
		},
		{
			name:    "migrate up does not take the flag down needs",
			args:    []string{"migrate", "up", "--i-know"},
			wantErr: "up does not read --i-know",
		},
		{
			name:    "migrate does not read a configuration repository",
			args:    []string{"migrate", "status", "--config", "./config"},
			wantErr: "status does not read --config",
		},
		{
			// down is the only action with a flag of its own, so this pins its
			// list from both sides: --i-know passes the gate, --config does not.
			name:    "migrate down takes --i-know and nothing else",
			args:    []string{"migrate", "down", "--i-know", "--config", "./config"},
			wantErr: "down does not read --config",
		},
		{
			// A flag behind the argument is a flag, not a second argument: the
			// two actions that take one would otherwise report "takes one
			// argument" about a command line that gave exactly one.
			name:    "l0 get takes a flag after the event id",
			args:    []string{"l0", "get", "evt:a:b", "--kind", "message"},
			wantErr: "get does not read --kind",
		},
		{
			name:    "migrate up-to takes a flag after the version",
			args:    []string{"migrate", "up-to", "1", "--i-know"},
			wantErr: "up-to does not read --i-know",
		},
		{
			name:    "l0 get still rejects a second argument",
			args:    []string{"l0", "get", "evt:a:b", "evt:c:d"},
			wantErr: "get takes one argument",
		},
		{
			name:    "migrate up-to still rejects a second argument",
			args:    []string{"migrate", "up-to", "1", "2"},
			wantErr: "up-to takes one argument",
		},
		{
			name:    "config validate takes a flag after the path",
			args:    []string{"config", "validate", "testdata/does-not-exist", "--log-level", "debug"},
			wantErr: "testdata/does-not-exist",
		},
		{
			// Looping past the words must not undo what `--` means. Port 1 is
			// reserved and nothing listens on it, so the id being taken as an
			// id rather than as a flag is what gets this to the connection.
			name:    "-- ends the flags, so an argument may look like one",
			args:    []string{"l0", "get", "--database-url", "postgres://h@127.0.0.1:1/d", "--", "-looks-like-a-flag"},
			wantErr: "connecting to postgres",
		},
		{
			// And it stays ended for the words behind the first: without that,
			// going round again would parse the second as a flag and report
			// "flag provided but not defined" for a command line whose real
			// problem is a second argument.
			name:    "-- keeps its meaning for every word behind it",
			args:    []string{"l0", "get", "--", "-one", "-two"},
			wantErr: "get takes one argument",
		},
		{
			// And the same when a flag precedes `--` in the same round: `fs.Parse`
			// consumes the terminator itself, so without splitting on it up
			// front the loop would go round again and report the second word
			// as an unknown flag instead of an unexpected argument.
			name:    "-- keeps its meaning behind a preceding flag too",
			args:    []string{"l0", "list", "--source", "s", "--", "-one", "-two"},
			wantErr: `unexpected argument "-one": list takes none`,
		},
		{
			name:    "l0 count rejects a stray argument",
			args:    []string{"l0", "count", "everything"},
			wantErr: `unexpected argument "everything": count takes none`,
		},
		{
			name:    "migrate status rejects a stray argument",
			args:    []string{"migrate", "status", "now"},
			wantErr: `unexpected argument "now": status takes none`,
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
	// `all` migrates the database it is pointed at (ADR-0006). These cases are
	// about the four services coming back, not about a schema, and the
	// integration-test check sets this variable for the whole run.
	t.Setenv("HEARSAY_DATABASE_URL", "")
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

// A connection URL carries a password, and `--help` goes to a terminal, a CI
// transcript or a screen share. The environment variable must not be the flag's
// default, because flag.PrintDefaults prints defaults.
func TestHelpDoesNotPrintTheDatabasePassword(t *testing.T) {
	const password = "correct-horse-battery-staple"
	t.Setenv("HEARSAY_DATABASE_URL", "postgres://hearsay:"+password+"@localhost:5432/hearsay")
	for _, args := range [][]string{
		{"l0", "--help"},
		{"migrate", "--help"},
		{"all", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(t.Context(), args, &stdout, &stderr); err != nil {
				t.Fatalf("run(%q) = %v, want nil", args, err)
			}
			if out := stdout.String() + stderr.String(); strings.Contains(out, password) {
				t.Errorf("--help printed the password:\n%s", out)
			}
			if !strings.Contains(stderr.String(), "database-url") {
				t.Errorf("--help does not mention --database-url:\n%s", stderr.String())
			}
		})
	}
}

// The flag still works, and still beats the environment.
func TestDatabaseURLFlagBeatsTheEnvironment(t *testing.T) {
	t.Setenv("HEARSAY_DATABASE_URL", "postgres://hearsay@localhost:5432/from-the-environment")
	var stdout, stderr bytes.Buffer
	// Port 1 is reserved and nothing listens on it, so this gets as far as the
	// connection and no further.
	err := run(t.Context(), []string{"l0", "count", "--database-url", "postgres://hearsay@127.0.0.1:1/from-the-flag"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run(l0 count) against a port nothing listens on = nil, want an error")
	}
	if !strings.Contains(err.Error(), "from-the-flag") {
		t.Errorf("run used %v, want the database the flag names", err)
	}
}

// Both subcommands that need Postgres say so rather than failing on a
// connection to nowhere, and both say it before doing anything else.
func TestSubcommandsThatNeedADatabaseSaySoWhenTheyHaveNone(t *testing.T) {
	for _, args := range [][]string{
		{"migrate", "status"},
		{"l0", "count"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// The integration-test check sets this for the whole run; these
			// cases are about the process that was given no database at all.
			t.Setenv("HEARSAY_DATABASE_URL", "")

			var stdout, stderr bytes.Buffer
			err := run(t.Context(), args, &stdout, &stderr)
			if !errors.Is(err, db.ErrNoDatabaseURL) {
				t.Fatalf("run(%q) = %v, want it to carry db.ErrNoDatabaseURL", args, err)
			}
			// run already wraps with the subcommand name, so the handler must not.
			if n := strings.Count(err.Error(), args[0]); n != 1 {
				t.Errorf("error names the subcommand %d times, want once: %v", n, err)
			}
		})
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
			// Most of what backs a model tier is usually a default that is
			// nowhere in the files, so the summary says what each one
			// resolved to.
			name:       "the summary says which model answers each tier",
			args:       []string{"config", "validate", valid},
			wantStdout: "model tiers",
		},
		{
			name:       "and names the provider and model it resolved to",
			args:       []string{"config", "validate", valid},
			wantStdout: "distill=anthropic/",
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
		// Cancelled up front, like the cases above: a service that ignored its
		// configuration would then return nil rather than block, so this fails
		// instead of hanging.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := run(ctx, []string{"api", "--config", broken}, &stdout, &stderr)
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
