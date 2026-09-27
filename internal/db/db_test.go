package db_test

import (
	"errors"
	"testing"

	"github.com/kpenfound/hearsay/internal/db"
)

// A process with no database configured says so, rather than failing on a
// connection to nowhere.
func TestNoDatabaseURL(t *testing.T) {
	if _, err := db.Open(t.Context(), ""); !errors.Is(err, db.ErrNoDatabaseURL) {
		t.Errorf("Open(%q) = %v, want db.ErrNoDatabaseURL", "", err)
	}
	if _, err := db.NewMigrator(t.Context(), "", nil); !errors.Is(err, db.ErrNoDatabaseURL) {
		t.Errorf("NewMigrator(%q) = %v, want db.ErrNoDatabaseURL", "", err)
	}
}

// Open proves the connection rather than handing back a pool that fails on
// first use: pgxpool does not connect until it is asked to, so without the
// check a database that is not there looks like a working one until a query
// reaches it.
func TestOpenFailsOnADatabaseThatIsNotThere(t *testing.T) {
	// Port 1 is reserved and nothing listens on it.
	if _, err := db.Open(t.Context(), "postgres://hearsay@127.0.0.1:1/hearsay?connect_timeout=2"); err == nil {
		t.Error("Open(a port nothing listens on) = nil, want an error")
	}
}
