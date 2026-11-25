package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config/configtest"
	"github.com/kpenfound/hearsay/internal/db"
)

type runCase struct {
	name       string
	args       []string
	wantErr    string
	wantStdout string
	wantStderr string
}

func assertRunCases(t *testing.T, tests []runCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
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

func TestRunUsageAndVersion(t *testing.T) {
	assertRunCases(t, []runCase{
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
			name:       "all help names worker probe flags",
			args:       []string{"all", "--help"},
			wantStderr: "-assert-worker-listen",
		},
		{
			name:       "a subcommand's --help is not an error",
			args:       []string{"connectors", "--help"},
			wantStderr: "-source",
		},
	})
}

func TestRunMigrateArguments(t *testing.T) {
	assertRunCases(t, []runCase{
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
			name:    "migrate rejects an unknown action",
			args:    []string{"migrate", "sideways"},
			wantErr: `unknown action "sideways"`,
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
			name:    "migrate down takes --i-know and nothing else",
			args:    []string{"migrate", "down", "--i-know", "--config", "./config"},
			wantErr: "down does not read --config",
		},
		{
			name:    "migrate up-to takes a flag after the version",
			args:    []string{"migrate", "up-to", "1", "--i-know"},
			wantErr: "up-to does not read --i-know",
		},
		{
			name:    "migrate up-to still rejects a second argument",
			args:    []string{"migrate", "up-to", "1", "2"},
			wantErr: "up-to takes one argument",
		},
		{
			name:    "migrate status rejects a stray argument",
			args:    []string{"migrate", "status", "now"},
			wantErr: `unexpected argument "now": status takes none`,
		},
	})
}

func TestRunL0Arguments(t *testing.T) {
	assertRunCases(t, []runCase{
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
			name:    "l0 get takes a flag after the event id",
			args:    []string{"l0", "get", "evt:a:b", "--kind", "message"},
			wantErr: "get does not read --kind",
		},
		{
			name:    "l0 get still rejects a second argument",
			args:    []string{"l0", "get", "evt:a:b", "evt:c:d"},
			wantErr: "get takes one argument",
		},
		{
			name:    "l0 count rejects a stray argument",
			args:    []string{"l0", "count", "everything"},
			wantErr: `unexpected argument "everything": count takes none`,
		},
	})
}

func TestRunFlagsMayFollowWordsAndDashDashEndsThem(t *testing.T) {
	assertRunCases(t, []runCase{
		{
			name:    "config validate takes a flag after the path",
			args:    []string{"config", "validate", "testdata/does-not-exist", "--log-level", "debug"},
			wantErr: "testdata/does-not-exist",
		},
		{
			name:    "-- ends the flags, so an argument may look like one",
			args:    []string{"l0", "get", "--database-url", "postgres://h@127.0.0.1:1/d", "--", "-looks-like-a-flag"},
			wantErr: "connecting to postgres",
		},
		{
			name:    "-- keeps its meaning for every word behind it",
			args:    []string{"l0", "get", "--", "-one", "-two"},
			wantErr: "get takes one argument",
		},
		{
			name:    "-- keeps its meaning behind a preceding flag too",
			args:    []string{"l0", "list", "--source", "s", "--", "-one", "-two"},
			wantErr: `unexpected argument "-one": list takes none`,
		},
	})
}

func TestRunServiceFlags(t *testing.T) {
	assertRunCases(t, []runCase{
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
			name:    "an empty --listen is rejected",
			args:    []string{"connectors", "--listen", ""},
			wantErr: "--listen is empty",
		},
		{
			name:    "distiller rejects empty listen",
			args:    []string{"distiller", "--listen", ""},
			wantErr: "--listen is empty",
		},
		{
			name:    "assert worker rejects empty listen",
			args:    []string{"assert-worker", "--listen", ""},
			wantErr: "--listen is empty",
		},
	})
}

func runCancelled(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = run(ctx, args, &out, &errOut)
	return out.String(), errOut.String(), err
}

func noDatabase(t *testing.T) {
	t.Helper()
	t.Setenv("HEARSAY_DATABASE_URL", "postgres://hearsay@127.0.0.1:1/unreachable")
}

func TestServiceReturnsNilWhenTheContextIsCancelled(t *testing.T) {
	noDatabase(t)
	done := make(chan error, 1)
	go func() {
		_, _, err := runCancelled(t, "api", "--log-format", "json")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run(api) = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run(api) did not return within 5s of cancellation")
	}
}

func TestServiceLogLinesCarryServiceVersionAndInstanceOnce(t *testing.T) {
	t.Setenv("HEARSAY_INSTANCE", "replica-7")
	noDatabase(t)
	_, stderr, err := runCancelled(t, "api", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	seen := false
	for line := range strings.Lines(strings.TrimSpace(stderr)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, field := range []string{"service", "version", "instance"} {
			if n := strings.Count(line, `"`+field+`":`); n != 1 {
				t.Errorf("log line has the %s field %d times, want once: %s", field, n, line)
			}
		}
		var rec struct {
			Service  string `json:"service"`
			Version  string `json:"version"`
			Instance string `json:"instance"`
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
		seen = seen || rec.Service == "api"
	}
	if !seen {
		t.Errorf("no log line from service api:\n%s", stderr)
	}
}

func TestInstanceDefaultsToTheHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	t.Setenv("HEARSAY_INSTANCE", "")
	noDatabase(t)

	_, stderr, err := runCancelled(t, "api", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if want := `"instance":"` + host + `"`; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %s", stderr, want)
	}
}

func TestLogFormatFromTheEnvironment(t *testing.T) {
	noDatabase(t)
	t.Setenv("HEARSAY_LOG_FORMAT", "text")
	_, stderr, err := runCancelled(t, "api")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if !strings.Contains(stderr, "service=api") {
		t.Errorf("stderr = %q, want text-format output", stderr)
	}
}

func TestLogFormatFlagBeatsTheEnvironment(t *testing.T) {
	noDatabase(t)
	t.Setenv("HEARSAY_LOG_FORMAT", "text")
	_, stderr, err := runCancelled(t, "api", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if !strings.Contains(stderr, `"service":"api"`) {
		t.Errorf("stderr = %q, want JSON-format output", stderr)
	}
}

func TestLogLevelFromTheEnvironment(t *testing.T) {
	noDatabase(t)
	t.Setenv("HEARSAY_LOG_LEVEL", "error")
	_, stderr, err := runCancelled(t, "api")
	if err != nil {
		t.Fatalf("run(api) = %v, want nil", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing below error level", stderr)
	}
}

func TestHelpDoesNotPrintTheDatabasePassword(t *testing.T) {
	const password = "correct-horse-battery-staple"
	t.Setenv("HEARSAY_DATABASE_URL", "postgres://hearsay:"+password+"@localhost:5432/hearsay")
	for _, args := range [][]string{
		{"l0", "--help"},
		{"migrate", "--help"},
		{"all", "--help"},
		{"connectors", "--help"},
		{"api", "--help"},
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

func TestDatabaseURLFlagBeatsTheEnvironment(t *testing.T) {
	t.Setenv("HEARSAY_DATABASE_URL", "postgres://hearsay@localhost:5432/from-the-environment")
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"l0", "count", "--database-url", "postgres://hearsay@127.0.0.1:1/from-the-flag"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run(l0 count) against a port nothing listens on = nil, want an error")
	}
	if !strings.Contains(err.Error(), "from-the-flag") {
		t.Errorf("run used %v, want the database the flag names", err)
	}
}

func TestSubcommandsThatNeedADatabaseSaySoWhenTheyHaveNone(t *testing.T) {
	for _, args := range [][]string{
		{"migrate", "status"},
		{"l0", "count"},
		{"distiller"},
		{"assert-worker"},
		{"connectors"},
		{"api"},
		{"all"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("HEARSAY_DATABASE_URL", "")

			var stdout, stderr bytes.Buffer
			err := run(t.Context(), args, &stdout, &stderr)
			if !errors.Is(err, db.ErrNoDatabaseURL) {
				t.Fatalf("run(%q) = %v, want it to carry db.ErrNoDatabaseURL", args, err)
			}
			if n := strings.Count(err.Error(), args[0]); n != 1 {
				t.Errorf("error names the subcommand %d times, want once: %v", n, err)
			}
		})
	}
}

var validConfig = map[string]string{
	"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
	"scopes/api.yaml":     "id: api\nsources: [github]\n",
}

func TestConfigValidateSummarisesAValidConfiguration(t *testing.T) {
	valid := configtest.Write(t, validConfig)
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"config", "validate", valid}, &stdout, &stderr); err != nil {
		t.Fatalf("config validate = %v", err)
	}
	for _, want := range []string{"is valid", "sha256:", "model tiers", "distill=anthropic/"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
}

func TestConfigValidateTakesThePathFromTheFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"config", "validate", "--config", configtest.Write(t, validConfig)}, &stdout, &stderr); err != nil {
		t.Fatalf("config validate = %v", err)
	}
	if !strings.Contains(stdout.String(), "is valid") {
		t.Errorf("stdout = %q, want it to say the configuration is valid", stdout.String())
	}
}

func TestConfigValidateRefusals(t *testing.T) {
	valid := configtest.Write(t, validConfig)
	broken := configtest.Write(t, map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":     "id: api\nsources: [github, discord]\n",
	})
	assertRunCases(t, []runCase{
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
	})
}

func TestServicesRejectAnAgentWithoutScopes(t *testing.T) {
	noDatabase(t)
	agentWithoutScopes := maps.Clone(validConfig)
	agentWithoutScopes["principals/agent.yaml"] = "id: shed\nkind: agent\nclass: worker\nidentities: [{source: github, handle: shed}]\n"
	missingAgentGrant := configtest.Write(t, agentWithoutScopes)
	for _, service := range []string{"api", "connectors", "distiller", "assert-worker"} {
		t.Run(service, func(t *testing.T) {
			_, _, err := runCancelled(t, service, "--config", missingAgentGrant)
			if err == nil || !strings.Contains(err.Error(), `principal "shed": scopes: is required`) {
				t.Fatalf("run(%s --config <agent without scopes>) = %v", service, err)
			}
		})
	}
}

func TestServiceLogsTheLoadedConfigurationWithItsDigest(t *testing.T) {
	noDatabase(t)
	_, stderr, err := runCancelled(t, "api", "--config", configtest.Write(t, validConfig), "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api --config <valid>) = %v, want no error", err)
	}
	for _, want := range []string{`"msg":"configuration loaded"`, `"config_digest":"sha256:`, `"sources":1`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %s", stderr, want)
		}
	}
}

func TestServiceReadsTheConfigurationPathFromTheEnvironment(t *testing.T) {
	noDatabase(t)
	t.Setenv("HEARSAY_CONFIG", configtest.Write(t, validConfig))
	_, stderr, err := runCancelled(t, "api", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want no error", err)
	}
	if !strings.Contains(stderr, `"msg":"configuration loaded"`) {
		t.Errorf("stderr = %q, want the configuration to have been loaded", stderr)
	}
}

func TestServiceWarnsRatherThanRefusesWithoutAConfiguration(t *testing.T) {
	noDatabase(t)
	t.Setenv("HEARSAY_CONFIG", "")
	_, stderr, err := runCancelled(t, "api", "--log-format", "json")
	if err != nil {
		t.Fatalf("run(api) = %v, want no error", err)
	}
	if !strings.Contains(stderr, `"level":"WARN"`) || !strings.Contains(stderr, "no configuration") {
		t.Errorf("stderr = %q, want a warning that there is no configuration", stderr)
	}
}
