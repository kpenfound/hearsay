package l2_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l2"
)

// trackerRepo maps acme/api in github-acme to a tracker, and covers acme/web
// without mapping it.
func trackerRepo() config.Repo {
	return config.Repo{Scopes: []config.Scope{
		{ID: "api", Tracker: config.SourceRef{Source: "github-acme", Project: "acme/api"}},
		{ID: "web"},
	}}
}

func issueEvent(kind connector.Kind, artifact, partOf string) connector.Event {
	return connector.Event{
		Source: "github-acme", NativeID: artifact, Kind: kind, Time: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
		Payload: connector.Payload{
			Artifact: artifact, PartOf: partOf,
			Container: connector.Container{Kind: connector.ContainerRepository, NativeID: "acme/api"},
		},
	}
}

func TestPlacementOf(t *testing.T) {
	item := func(n string) string { return "tracker:github-acme:acme/api#" + n }
	tests := []struct {
		name    string
		ev      connector.Event
		ok      bool
		item    string
		parents []string
	}{
		{name: "a sub-issue of a mapped issue", ev: issueEvent(connector.KindIssue, "acme/api#11", "acme/api#10"),
			ok: true, item: item("11"), parents: []string{item("10")}},
		{name: "an issue with no parent is placed nowhere", ev: issueEvent(connector.KindIssue, "acme/api#12", ""),
			ok: true, item: item("12")},
		{name: "a parent no tracker maps is no entity", ev: issueEvent(connector.KindIssue, "acme/api#14", "acme/web#7"),
			ok: true, item: item("14")},
		{name: "an extension kind that is an issue", ev: func() connector.Event {
			ev := issueEvent("jira.story", "acme/api#11", "acme/api#10")
			ev.Payload.BaseKind = connector.KindIssue
			return ev
		}(), ok: true, item: item("11"), parents: []string{item("10")}},
		{name: "an issue no tracker maps", ev: issueEvent(connector.KindIssue, "acme/web#3", "acme/api#10")},
		{name: "an issue in another source", ev: func() connector.Event {
			ev := issueEvent(connector.KindIssue, "acme/api#11", "acme/api#10")
			ev.Source = "github-oss"
			return ev
		}()},
		{name: "a pull request", ev: issueEvent(connector.KindPullRequest, "acme/api#2", "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := l2.PlacementOf(trackerRepo(), tt.ev)
			if ok != tt.ok || p.Item.ID != tt.item || !slices.Equal(p.ParentIDs(), tt.parents) {
				t.Errorf("PlacementOf() = %s under %v, %v; want %s under %v, %v", p.Item.ID, p.ParentIDs(), ok, tt.item, tt.parents, tt.ok)
			}
			if ok && (p.Item.Type != l2.TypeTrackerItem || p.Item.Origin != l2.OriginReference || p.Item.Name != tt.ev.Payload.Artifact) {
				t.Errorf("item = %+v, want a tracker item named %s", p.Item, tt.ev.Payload.Artifact)
			}
		})
	}
}

// Issue #120: seeding merges the tracker's hierarchy at its rank (ADR-0016):
// under configuration, over repository structure, with a cycle's closing edge
// dropped. A placement with no parent adds no entity.
func TestSeedMergesTheTrackersHierarchy(t *testing.T) {
	repo := seedRepo()
	repo.Scopes = trackerRepo().Scopes
	placed := func(artifact, partOf string) l2.Placement {
		p, ok := l2.PlacementOf(repo, issueEvent(connector.KindIssue, artifact, partOf))
		if !ok {
			t.Fatalf("PlacementOf(%s) is not ok", artifact)
		}
		return p
	}
	item := func(n string) string { return "tracker:github-acme:acme/api#" + n }
	tracker := []l2.Placement{
		placed("acme/api#11", "acme/api#10"),
		placed("acme/api#12", "acme/api#11"),
		// Closes a cycle, and loses: #10 is taken first, by id, and #12's
		// edge would close it.
		placed("acme/api#10", "acme/api#12"),
		placed("acme/api#13", ""),
		placed("acme/api#14", "acme/web#7"),
		// Not what a tracker says about code, but what the rank does if it
		// did: configuration's parent stands, and the tracker's replaces the
		// one repository structure implies.
		{Item: l2.Entity{ID: "code:acme/api:engine/server"}, Parents: []l2.Entity{{ID: item("10")}}},
		{Item: l2.Entity{ID: "code:acme/api:queue"}, Parents: []l2.Entity{{ID: item("10")}}},
	}

	ctx, log := seedLog(t)
	got, err := l2.Seed(ctx, repo, &fakeRepos{top: map[string][]string{"acme/api": {"engine", "internal"}}}, tracker)
	if err != nil {
		t.Fatalf("Seed() = %v", err)
	}
	parents := map[string][]string{}
	for _, e := range got {
		parents[e.ID] = e.PartOf
	}
	want := map[string][]string{
		"code:acme/api":               nil,
		"code:acme/api:engine":        {"code:acme/api"},
		"code:acme/api:engine/client": {"code:acme/api:engine"},
		"code:acme/api:engine/server": {"code:acme/api:engine"},
		"code:acme/api:internal":      {"code:acme/api"},
		"code:acme/api:queue":         {item("10")},
		item("10"):                    {item("12")},
		item("11"):                    {item("10")},
		item("12"):                    nil,
	}
	if fmt.Sprint(parents) != fmt.Sprint(want) {
		t.Errorf("Seed() part_of =\n%v\nwant\n%v", parents, want)
	}
	if d := droppedIn(t, log.String()); fmt.Sprint(d) != fmt.Sprint([]dropped{{item("12"), item("11"), "tracker"}}) {
		t.Errorf("logged dropped edges %v, want #12's edge to #11", d)
	}
	for _, e := range got {
		if e.ID == item("11") && (e.Type != l2.TypeTrackerItem || e.Origin != l2.OriginReference || e.Name != "acme/api#11") {
			t.Errorf("seeded %+v, want a tracker item", e)
		}
	}
}
