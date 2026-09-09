package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// This package's README opens with the table of subcommands, and CLAUDE.md sends
// every agent to it before they touch the package. Nothing else checks it: no
// Dagger check reads a Markdown file, so a subcommand added without a row, or a
// paragraph dropped into the middle of the table — a blank line ends one, and
// the rows below it stop being rows — ships green.
func TestREADMEListsEverySubcommand(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	listed := subcommandsInFirstTable(t, string(readme))
	if len(listed) == 0 {
		t.Fatal("README.md has no table of subcommands")
	}
	for _, cmd := range commands() {
		if cmd.name == "help" {
			// `help` is how the table is read, not a row in it.
			continue
		}
		if !slices.Contains(listed, cmd.name) {
			t.Errorf("subcommand %q has no row in README.md's table; it lists %v", cmd.name, listed)
		}
	}
}

// subcommandsInFirstTable returns the subcommand named by each row of the first
// Markdown table in the document. It stops at the first line that is not a row,
// which is the whole point: a table interrupted by anything ends there.
func subcommandsInFirstTable(t *testing.T, doc string) []string {
	t.Helper()
	var names []string
	inTable := false
	for line := range strings.Lines(doc) {
		line = strings.TrimSpace(line)
		row := strings.HasPrefix(line, "|")
		switch {
		case !inTable && !row:
			continue
		case !inTable:
			// The header, then its delimiter, then the rows.
			inTable = true
		case !row:
			return names
		case strings.HasPrefix(line, "|---"):
		default:
			if name, ok := subcommandOf(line); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// subcommandOf pulls the subcommand out of a row's first cell, which reads
// `| `+"`hearsay <name> ...`"+` | ... |`.
func subcommandOf(row string) (string, bool) {
	cell, _, ok := strings.Cut(strings.TrimPrefix(row, "|"), "|")
	if !ok {
		return "", false
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(strings.ReplaceAll(cell, "`", "")), "hearsay ")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, " ")
	return name, name != ""
}
