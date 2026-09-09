package db_test

import (
	"errors"
	"strings"
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

// A connection URL carries a password, and an error message goes to a log, a
// terminal or a CI transcript. Nothing here may put it there.
func TestAConnectionErrorDoesNotCarryThePassword(t *testing.T) {
	const password = "correct-horse-battery-staple"
	// A port that is not a number: pgx rejects it while parsing, which is the
	// path that has the whole URL in its hands.
	url := "postgres://hearsay:" + password + "@localhost:notaport/hearsay"
	_, err := db.Open(t.Context(), url)
	if err == nil {
		t.Fatalf("Open(%q) = nil, want an error", url)
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("the error quotes the password: %v", err)
	}
}
