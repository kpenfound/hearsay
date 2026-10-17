//go:build integration

package assertworker_test

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

func TestWorkEntitiesAtStartupAndWhileFollowing(t *testing.T) {
	for _, mode := range []string{"startup", "follower"} {
		t.Run(mode, func(t *testing.T) {
			pool := scratchPool(t)
			repo := config.Repo{Sources: []connector.SourceConfig{{ID: "projects", Type: "work"}, {ID: "other-projects", Type: "work"}}, Code: []config.CodeEntity{{ID: "code:acme/api", Type: config.TypeProject}, {ID: "code:acme/web", Type: config.TypeProject}}}
			var logs bytes.Buffer
			ctx := telemetry.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
			appended, processed := 0, 0
			sync := func() {
				t.Helper()
				if mode == "startup" {
					if _, err := assertworker.SeedEntities(ctx, pool, repo, nil); err != nil {
						t.Fatal(err)
					}
				} else {
					f := assertworker.NewFollower(pool, repo, time.Millisecond, l0.MaxLimit)
					deadline := time.Now().Add(20 * time.Second)
					for processed < appended {
						n, err := f.Once(ctx)
						if err != nil {
							t.Fatal(err)
						}
						processed += n
						if n == 0 {
							if time.Now().After(deadline) {
								t.Fatalf("feed processed %d of %d events", processed, appended)
							}
							// Other databases' unfinished transactions can hold the feed's xmin.
							time.Sleep(5 * time.Millisecond)
						}
					}
				}
			}
			// A follower starts after the normal code seed. Runtime entities need no tracker.
			if _, err := assertworker.SeedEntities(ctx, pool, repo, nil); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			project := connector.Event{Source: "projects", Kind: connector.KindIssue, Time: now, NativeID: "p@one", ACL: connector.ACL{{Kind: connector.ACLPublic}}, Payload: connector.Payload{Author: &connector.Identity{Source: "projects", Kind: connector.IdentityAgent, NativeID: "factory"}, Artifact: "p", Title: "Initial project", Container: connector.Container{Kind: "project", NativeID: "p"}, Revision: &connector.Revision{Token: "one", EditedAt: now}, Touches: []connector.Touch{{Source: "github", Project: "acme/api"}, {Source: "github", Project: "acme/web"}, {Source: "github", Project: "acme/missing"}}}}
			stream := project
			stream.NativeID = "p#s@one"
			stream.Payload.Artifact = "p#s"
			stream.Payload.Title = "Initial stream"
			stream.Payload.PartOf = "p"
			stream.Payload.Touches = nil
			appendEvent := func(ev connector.Event) {
				t.Helper()
				if _, err := l0.New(pool).Append(ctx, ev); err != nil {
					t.Fatal(err)
				}
				appended++
			}
			appendEvent(project)
			appendEvent(stream)
			other := project
			other.Source = "other-projects"
			appendEvent(other)
			sync()
			check := func(id, name string, kind l2.EntityType, parents ...string) {
				t.Helper()
				e, err := l2.New(pool).Entity(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if e.Name != name || e.Type != kind || e.Origin != l2.OriginSource || len(e.Aliases) != 0 || !slices.Equal(e.PartOf, parents) {
					t.Fatalf("entity = %+v, want %s %v", e, name, parents)
				}
			}
			check("work:projects:p", "Initial project", l2.TypeProject, "code:acme/api", "code:acme/web")
			check("work:projects:p#s", "Initial stream", l2.TypeTrackerItem, "work:projects:p")
			check("work:other-projects:p", "Initial project", l2.TypeProject, "code:acme/api", "code:acme/web")
			if !strings.Contains(logs.String(), `"level":"INFO"`) || !strings.Contains(logs.String(), `"parent":"code:acme/missing"`) || strings.Contains(logs.String(), "Initial project") {
				t.Fatalf("missing-parent log must contain ids only: %s", logs.String())
			}
			for _, original := range []connector.Event{project, stream} {
				ev := original
				ev.NativeID = ev.Payload.Artifact + "@two"
				ev.Payload.Revision = &connector.Revision{Token: "two", EditedAt: now.Add(time.Hour)}
				ev.Payload.Title = "Renamed " + original.Payload.Title
				appendEvent(ev)
				// A late revision must never put the old name back.
				late := original
				late.NativeID = late.Payload.Artifact + "@late"
				late.Payload.Revision = &connector.Revision{Token: "late", EditedAt: now.Add(30 * time.Minute)}
				appendEvent(late)
			}
			sync()
			check("work:projects:p", "Renamed Initial project", l2.TypeProject, "code:acme/api", "code:acme/web")
			check("work:projects:p#s", "Renamed Initial stream", l2.TypeTrackerItem, "work:projects:p")
			tombstone := project
			tombstone.Kind = connector.KindTombstone
			tombstone.NativeID = "p:tombstone"
			tombstone.Payload = connector.Payload{Artifact: "p:tombstone", Target: "p", Container: project.Payload.Container}
			appendEvent(tombstone)
			sync()
			if _, err := l2.New(pool).Entity(ctx, "work:projects:p"); !errors.Is(err, l2.ErrNotFound) {
				t.Fatalf("deleted project: %v", err)
			}
			check("work:projects:p#s", "Renamed Initial stream", l2.TypeTrackerItem)
			tombstone.NativeID = "p#s:tombstone"
			tombstone.Payload.Target = "p#s"
			tombstone.Payload.Artifact = "p#s:tombstone"
			appendEvent(tombstone)
			sync()
			if _, err := l2.New(pool).Entity(ctx, "work:projects:p#s"); !errors.Is(err, l2.ErrNotFound) {
				t.Fatalf("deleted stream: %v", err)
			}
		})
	}
}
