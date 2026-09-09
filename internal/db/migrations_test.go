package db_test

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/db"
)

// The rules internal/db/migrations/README.md states, checked against the files
// rather than trusted. A migration goose cannot version, or one with no down
// section, is only discovered at a deployment otherwise.
func TestEveryMigrationIsWellFormed(t *testing.T) {
	entries, err := fs.ReadDir(db.Migrations(), ".")
	if err != nil {
		t.Fatalf("reading the embedded migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations are embedded; the go:embed pattern has stopped matching")
	}

	previous := int64(0)
	for _, entry := range entries {
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			if !strings.HasSuffix(name, ".sql") {
				t.Fatalf("%q is not a .sql file; migrations are plain SQL (ADR-0006)", name)
			}
			digits, description, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
			if !ok || description == "" {
				t.Fatalf("%q is not named NNNNN_short_description.sql", name)
			}
			version, err := strconv.ParseInt(digits, 10, 64)
			if err != nil || version <= 0 {
				t.Fatalf("%q does not start with a version number", name)
			}
			if version <= previous {
				t.Fatalf("version %d does not follow %d; versions are unique and ascending", version, previous)
			}
			previous = version

			body, err := fs.ReadFile(db.Migrations(), name)
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			for _, annotation := range []string{"-- +goose Up", "-- +goose Down"} {
				if !strings.Contains(string(body), annotation) {
					t.Errorf("%s has no %q section", name, annotation)
				}
			}
		})
	}

	version, err := db.EmbeddedVersion()
	if err != nil {
		t.Fatalf("EmbeddedVersion() = %v, want no error", err)
	}
	if version != previous {
		t.Errorf("EmbeddedVersion() = %d, want %d, the newest migration on disk", version, previous)
	}
}

// ADR-0006 makes the vector extension the first migration, so that every later
// one may declare a column of that type and a Postgres without pgvector fails
// on the one thing that is missing.
func TestTheFirstMigrationCreatesTheVectorExtension(t *testing.T) {
	entries, err := fs.ReadDir(db.Migrations(), ".")
	if err != nil {
		t.Fatalf("reading the embedded migrations: %v", err)
	}
	first := entries[0].Name()
	body, err := fs.ReadFile(db.Migrations(), first)
	if err != nil {
		t.Fatalf("reading %s: %v", first, err)
	}
	up, _, _ := strings.Cut(string(body), "-- +goose Down")
	if !strings.Contains(up, "CREATE EXTENSION IF NOT EXISTS vector") {
		t.Errorf("the first migration is %s, and it does not create the vector extension (ADR-0006)", first)
	}
}
