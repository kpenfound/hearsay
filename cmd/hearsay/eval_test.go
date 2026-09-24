package main

import (
	"bytes"
	"strings"
	"testing"
)

// Everything `hearsay eval` refuses on its command line is refused before the
// database is opened: an unreachable one does not change the answer, and
// nothing is printed.
func TestEvalRefusesBeforeOpeningTheDatabase(t *testing.T) {
	configPath := writeConfig(t, validConfig)
	common := []string{"--database-url", "postgres://hearsay@nowhere.invalid:1/hearsay", "--config", configPath}
	with := func(args ...string) []string { return append(append([]string{}, common...), args...) }
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"an action word", with("ratification"), `unexpected argument "ratification"`},
		{"--since that is not RFC3339", with("--since", "2026-09-01"), `--since "2026-09-01" is not an RFC3339 time`},
		{"--until that is not RFC3339", with("--until", "tomorrow"), `--until "tomorrow" is not an RFC3339 time`},
		{"--since after --until", with("--since", "2026-09-02T00:00:00Z", "--until", "2026-09-01T00:00:00Z"), "is not before --until"},
		{"--since equal to --until", with("--since", "2026-09-01T00:00:00Z", "--until", "2026-09-01T00:00:00Z"), "is not before --until"},
		{"--since in the future", with("--since", "2999-01-01T00:00:00Z"), "is not before --until"},
		{"a flag it does not have", with("--principal", "kyle"), "flag provided but not defined: -principal"},
		{"no config", []string{"--database-url", "postgres://hearsay@nowhere.invalid:1/hearsay"}, "eval needs --config"},
		{"an invalid config", []string{"--database-url", "postgres://hearsay@nowhere.invalid:1/hearsay", "--config", t.TempDir() + "/missing"}, "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HEARSAY_CONFIG", "")
			var out, stderr bytes.Buffer
			err := run(t.Context(), append([]string{"eval"}, tt.args...), &out, &stderr)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run() = %v, want an error saying %q", err, tt.want)
			}
			if out.Len() != 0 {
				t.Errorf("a refusal printed %q", out.String())
			}
		})
	}
}
