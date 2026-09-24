package main

import (
	"bytes"
	"strings"
	"testing"
)

// Everything `hearsay topics` refuses on its command line or its principal is
// refused before the database is opened: an unreachable one does not change
// the answer, and nothing is printed.
func TestTopicsRefusesBeforeOpeningTheDatabase(t *testing.T) {
	configPath := writeConfig(t, map[string]string{
		"sources/github.yaml":  "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":      "id: api\nsources: [github]\n",
		"principals/kyle.yaml": "id: kyle\nidentities: [{source: github, native_id: u1}]\n",
		"principals/bot.yaml":  "id: bot\nkind: agent\nclass: steward\nscopes: ['*']\nidentities: [{source: github, native_id: b1}]\n",
		"principals/team.yaml": "id: team\nkind: team\nmembers: [kyle]\n",
	})
	common := []string{"--database-url", "postgres://hearsay@nowhere.invalid:1/hearsay", "--config", configPath}
	as := func(who string, args ...string) []string {
		return append(append(append([]string{}, common...), "--principal", who), args...)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no action", as("kyle"), "no action given: want list, merge, split, undo or ops"},
		{"unknown action", as("kyle", "rename", "topic:a"), `unknown action "rename"`},
		{"list without a scope", as("kyle", "list"), "list takes one scope"},
		{"list with two scopes", as("kyle", "list", "api", "web"), "list takes one scope"},
		{"list with --json", as("kyle", "list", "api", "--json"), "list does not read --json"},
		{"list with --scope", as("kyle", "list", "api", "--scope", "api"), "list does not read --scope"},
		{"merge with one topic", as("kyle", "merge", "topic:a"), "merge takes the topic to merge and the topic it goes into"},
		{"merge with three topics", as("kyle", "merge", "topic:a", "topic:b", "topic:c"), "merge takes the topic to merge"},
		{"merge with --name", as("kyle", "merge", "topic:a", "topic:b", "--name", "x"), "merge does not read --name"},
		{"merge with --stance", as("kyle", "merge", "topic:a", "topic:b", "--stance", "s"), "merge does not read --stance"},
		{"split without a topic", as("kyle", "split", "--stance", "s", "--name", "x"), "split takes one topic"},
		{"split without a stance", as("kyle", "split", "topic:a", "--name", "x"), "split needs --stance"},
		{"split with an empty stance", as("kyle", "split", "topic:a", "--stance", "", "--name", "x"), "empty value"},
		{"split without a name", as("kyle", "split", "topic:a", "--stance", "s"), "split needs --name"},
		{"split with a blank name", as("kyle", "split", "topic:a", "--stance", "s", "--name", "  "), "split needs --name"},
		{"split with --json", as("kyle", "split", "topic:a", "--stance", "s", "--name", "x", "--json"), "split does not read --json"},
		{"undo without an operation", as("kyle", "undo"), "undo takes one operation id"},
		{"undo with two operations", as("kyle", "undo", "1", "2"), "undo takes one operation id"},
		{"undo of a word", as("kyle", "undo", "latest"), `"latest" is not an operation id`},
		{"undo of zero", as("kyle", "undo", "0"), `"0" is not an operation id`},
		{"undo of a negative id", as("kyle", "undo", "--", "-3"), `"-3" is not an operation id`},
		{"undo with --since", as("kyle", "undo", "1", "--since", "2026-09-01T00:00:00Z"), "undo does not read --since"},
		{"ops with an argument", as("kyle", "ops", "api"), `unexpected argument "api": ops takes none`},
		{"ops with --stance", as("kyle", "ops", "--stance", "s"), "ops does not read --stance"},
		{"ops with a date that is not RFC3339", as("kyle", "ops", "--since", "2026-09-01"), `--since "2026-09-01" is not an RFC3339 time`},
		{"no principal", append(append([]string{}, common...), "list", "api"), "topics needs --config and --principal"},
		{"no config", []string{"--database-url", "postgres://hearsay@nowhere.invalid:1/hearsay", "--principal", "kyle", "list", "api"}, "topics needs --config and --principal"},
		{"unknown principal", as("nobody", "list", "api"), `unknown principal "nobody"`},
		{"agent principal", as("bot", "merge", "topic:a", "topic:b"), `principal "bot" is of kind "agent": topics are read and changed by a configured human`},
		{"agent principal reading", as("bot", "ops"), `principal "bot" is of kind "agent"`},
		{"team principal", as("team", "list", "api"), `principal "team" is of kind "team"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HEARSAY_CONFIG", "")
			var out, stderr bytes.Buffer
			err := run(t.Context(), append([]string{"topics"}, tt.args...), &out, &stderr)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run() = %v, want an error saying %q", err, tt.want)
			}
			if out.Len() != 0 {
				t.Errorf("a refusal printed %q", out.String())
			}
		})
	}
}
