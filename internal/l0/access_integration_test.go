//go:build integration

package l0_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

// ContainerAccess's bulk fixture must not enter the shared distiller feed:
// its artifacts would fill a page of pending jobs in other packages' tests.
func newScratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("HEARSAY_DATABASE_URL")
	if adminURL == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	ctx := t.Context()
	admin, err := db.Open(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "l0_access_" + newSourceID(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		dropper, err := db.Open(ctx, adminURL)
		if err != nil {
			t.Error(err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	base, query, hasQuery := strings.Cut(adminURL, "?")
	url := base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		url += "?" + query
	}
	m, err := db.NewMigrator(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if _, err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestContainerAccessUsesEveryDistinctCurrentList(t *testing.T) {
	pool := newScratchPool(t)
	public := connector.ACL{{Kind: connector.ACLPublic}}
	for _, tt := range []struct {
		name                    string
		setup                   func(*testing.T, *l0.Store, *connector.Fake, connector.ACL)
		wantPublic, wantPrivate bool
	}{
		{"empty container", func(*testing.T, *l0.Store, *connector.Fake, connector.ACL) {}, false, false},
		{"duplicate lists", func(t *testing.T, s *l0.Store, f *connector.Fake, private connector.ACL) {
			for _, id := range []string{"one", "two"} {
				appendAccessEvent(t, s, f, id, "repo", "", 0, public)
			}
		}, true, false},
		{"mixed resync", func(t *testing.T, s *l0.Store, f *connector.Fake, private connector.ACL) {
			appendAccessEvent(t, s, f, "one", "repo", "", 0, public)
			appendAccessEvent(t, s, f, "two", "repo", "", 0, private)
		}, true, true},
		{"older public revision is excluded", func(t *testing.T, s *l0.Store, f *connector.Fake, private connector.ACL) {
			appendAccessEvent(t, s, f, "one", "repo", "", 0, public)
			appendAccessEvent(t, s, f, "one", "repo", "new", 2, private)
			appendAccessEvent(t, s, f, "one", "repo", "late-old", 1, public)
		}, false, true},
		{"arrival breaks edit time ties", func(t *testing.T, s *l0.Store, f *connector.Fake, private connector.ACL) {
			appendAccessEvent(t, s, f, "one", "repo", "first", 1, public)
			appendAccessEvent(t, s, f, "one", "repo", "second", 1, private)
		}, false, true},
		{"moved artifacts do not prove their former container", func(t *testing.T, s *l0.Store, f *connector.Fake, private connector.ACL) {
			appendAccessEvent(t, s, f, "one", "repo", "", 0, public)
			appendAccessEvent(t, s, f, "one", "elsewhere", "moved", 1, private)
		}, false, false},
		{"tombstones are not current contents", func(t *testing.T, s *l0.Store, f *connector.Fake, private connector.ACL) {
			appendAccessEvent(t, s, f, "one", "repo", "", 0, public)
			tomb := f.NewEvent(connector.KindTombstone, "one:deleted", "")
			tomb.Payload.Target = "one"
			tomb.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: "repo"}
			if _, err := s.Append(t.Context(), tomb); err != nil {
				t.Fatal(err)
			}
		}, false, false},
		{"restored artifact proves access again", func(t *testing.T, s *l0.Store, f *connector.Fake, private connector.ACL) {
			appendAccessEvent(t, s, f, "one", "repo", "", 0, public)
			tomb := f.NewEvent(connector.KindTombstone, "one:deleted", "")
			tomb.Payload.Target = "one"
			if _, err := s.Append(t.Context(), tomb); err != nil {
				t.Fatal(err)
			}
			appendAccessEvent(t, s, f, "one", "repo", "restored", 1, private)
		}, false, true},
		{"not capped at a page", func(t *testing.T, s *l0.Store, f *connector.Fake, private connector.ACL) {
			for i := range l0.MaxLimit + 1 {
				appendAccessEvent(t, s, f, fmt.Sprintf("item%d", i), "repo", "", 0, public)
			}
			appendAccessEvent(t, s, f, "last", "repo", "", 0, private)
		}, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, source := newFake(t)
			s := l0.New(pool)
			private := connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: "team"}}
			tt.setup(t, s, f, private)
			// Neither another source nor another container contributes lists.
			appendAccessEvent(t, s, f, "unrelated", "other", "", 0, public)
			got, err := s.ContainerAccess(t.Context(), source, "repo")
			if err != nil {
				t.Fatal(err)
			}
			var foundPublic, foundPrivate bool
			for _, acl := range got {
				switch {
				case acl.Equal(public):
					foundPublic = true
				case acl.Equal(private):
					foundPrivate = true
				default:
					t.Fatalf("unexpected access list %+v", acl)
				}
			}
			wantCount := 0
			if tt.wantPublic {
				wantCount++
			}
			if tt.wantPrivate {
				wantCount++
			}
			if foundPublic != tt.wantPublic || foundPrivate != tt.wantPrivate || len(got) != wantCount {
				t.Fatalf("lists = %+v, want public=%v private=%v, each once", got, tt.wantPublic, tt.wantPrivate)
			}
		})
	}
}

func appendAccessEvent(t *testing.T, s *l0.Store, f *connector.Fake, artifact, container, token string, hour int, acl connector.ACL) {
	t.Helper()
	event := f.NewEvent(connector.KindIssue, artifact, "an artifact")
	event.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: container}
	event.ACL = acl
	if token != "" {
		event.NativeID = artifact + "@" + token
		event.Payload.Revision = &connector.Revision{Token: token, EditedAt: event.Time.Add(time.Duration(hour) * time.Hour)}
	}
	if _, err := s.Append(t.Context(), event); err != nil {
		t.Fatal(err)
	}
}
