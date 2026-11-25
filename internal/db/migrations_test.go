package db_test

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/db"
)

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
				t.Fatalf("%q is not a .sql file; migrations are plain SQL", name)
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
}

func TestEmbeddedVersionIsTheNewestMigration(t *testing.T) {
	entries, err := fs.ReadDir(db.Migrations(), ".")
	if err != nil {
		t.Fatalf("reading the embedded migrations: %v", err)
	}
	digits, _, _ := strings.Cut(entries[len(entries)-1].Name(), "_")
	want, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		t.Fatalf("newest migration %q has no version: %v", entries[len(entries)-1].Name(), err)
	}
	got, err := db.EmbeddedVersion()
	if err != nil {
		t.Fatalf("EmbeddedVersion() = %v, want no error", err)
	}
	if got != want {
		t.Errorf("EmbeddedVersion() = %d, want %d, the newest migration on disk", got, want)
	}
}
