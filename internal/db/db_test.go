package db_test

import (
	"errors"
	"testing"

	"github.com/kpenfound/hearsay/internal/db"
)

func TestOpenWithoutADatabaseURL(t *testing.T) {
	if _, err := db.Open(t.Context(), ""); !errors.Is(err, db.ErrNoDatabaseURL) {
		t.Errorf("Open(%q) = %v, want db.ErrNoDatabaseURL", "", err)
	}
}

func TestNewMigratorWithoutADatabaseURL(t *testing.T) {
	if _, err := db.NewMigrator(t.Context(), "", nil); !errors.Is(err, db.ErrNoDatabaseURL) {
		t.Errorf("NewMigrator(%q) = %v, want db.ErrNoDatabaseURL", "", err)
	}
}

func TestOpenFailsOnADatabaseThatIsNotThere(t *testing.T) {
	if _, err := db.Open(t.Context(), "postgres://hearsay@127.0.0.1:1/hearsay?connect_timeout=2"); err == nil {
		t.Error("Open(a port nothing listens on) = nil, want an error")
	}
}
