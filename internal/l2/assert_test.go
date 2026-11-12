package l2_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

func scopedRepo() config.Repo {
	return config.Repo{Scopes: []config.Scope{
		{ID: "web", Sources: []config.ScopeSource{{Source: "github-acme", Containers: []string{"*"}}}},
		{ID: "api", Sources: []config.ScopeSource{{Source: "github-acme", Containers: []string{"acme/api"}}},
			Tracker: config.SourceRef{Source: "github-acme", Project: "acme/api"}},
		{ID: "ops", Sources: []config.ScopeSource{{Source: "github-other", Containers: []string{"acme/ops"}}}},
	}}
}

func TestScopeKey(t *testing.T) {
	tests := []struct {
		name              string
		source, container string
		want              string
	}{
		{"the first covering scope by id", "github-acme", "acme/api", "api"},
		{"a wildcard container covers", "github-acme", "acme/web", "web"},
		{"a scope of another source does not cover", "github-other", "acme/api", "source:github-other"},
		{"a covered container of another source", "github-other", "acme/ops", "ops"},
		{"nothing covers an empty container", "github-acme", "", "source:github-acme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l2.ScopeKey(scopedRepo(), tt.source, tt.container, nil); got != tt.want {
				t.Errorf("ScopeKey(%q, %q) = %q, want %q", tt.source, tt.container, got, tt.want)
			}
		})
	}
}

func doc(kind l1.Kind, native string, outcome l1.OutcomeKind, refs ...l1.Reference) l1.Document {
	return l1.Document{
		ID:         l1.DocID("github-acme", native),
		Kind:       kind,
		Source:     l1.Source{System: "github-acme", NativeID: native},
		References: refs,
		Body:       l1.Body{OutcomeKind: outcome},
	}
}

func TestJoinKeys(t *testing.T) {
	tests := []struct {
		name string
		doc  l1.Document
		want []string
	}{
		{
			name: "an issue is its own join key",
			doc:  doc(l1.KindIssue, "acme/api#12", l1.OutcomeProposed),
			want: []string{"item:acme/api#12"},
		},
		{
			name: "a pull request joins what it references, in one item space",
			doc: doc(l1.KindPR, "acme/api#31", l1.OutcomeResolved,
				l1.Reference{Type: l1.RefIssue, ID: "acme/api#12"},
				l1.Reference{Type: l1.RefTrackerItem, ID: "acme/api#40"},
				l1.Reference{Type: l1.RefPR, ID: "acme/api#12"}),
			want: []string{"item:acme/api#12", "item:acme/api#31", "item:acme/api#40"},
		},
		{
			name: "commits and links are keys, people and systems are not",
			doc: doc(l1.KindCommit, "acme/api@abc", l1.OutcomeResolved,
				l1.Reference{Type: l1.RefCommit, ID: "acme/api@def"},
				l1.Reference{Type: l1.RefURL, ID: "https://example.com/rfc"},
				l1.Reference{Type: l1.RefPerson, ID: "kyle"},
				l1.Reference{Type: l1.RefSystem, ID: "code:acme/api:engine"}),
			want: []string{"commit:acme/api@abc", "commit:acme/api@def", "url:https://example.com/rfc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l2.JoinKeys(tt.doc); !slices.Equal(got, tt.want) {
				t.Errorf("JoinKeys() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTrackerItems(t *testing.T) {
	d := doc(l1.KindPR, "acme/api#31", l1.OutcomeResolved,
		l1.Reference{Type: l1.RefIssue, ID: "acme/api#12"},
		l1.Reference{Type: l1.RefTrackerItem, ID: "acme/web#3"}, // no tracker maps acme/web
		l1.Reference{Type: l1.RefCommit, ID: "acme/api@abc"})
	var got []string
	for _, e := range l2.TrackerItems(scopedRepo(), d) {
		if e.Type != l2.TypeTrackerItem || e.Origin != l2.OriginReference || !slices.Equal(e.Aliases, []string{e.Name}) {
			t.Errorf("TrackerItems() made %+v, want a tracker_item from a reference aliased by its own name", e)
		}
		got = append(got, e.ID)
	}
	want := []string{"tracker:github-acme:acme/api#12", "tracker:github-acme:acme/api#31"}
	if !slices.Equal(got, want) {
		t.Errorf("TrackerItems() = %q, want %q", got, want)
	}

	// The same repository in another source is another tracker.
	d.Source.System = "github-other"
	if got := l2.TrackerItems(scopedRepo(), d); len(got) != 0 {
		t.Errorf("TrackerItems(from another source) = %+v, want none", got)
	}
}

func TestIDsAreDerivedFromEveryPart(t *testing.T) {
	base := l2.TopicID("api", "l1:s:a", 0, "name")
	if again := l2.TopicID("api", "l1:s:a", 0, "name"); again != base {
		t.Errorf("TopicID is not stable: %q then %q", base, again)
	}
	for _, other := range []string{
		l2.TopicID("web", "l1:s:a", 0, "name"),
		l2.TopicID("api", "l1:s:b", 0, "name"),
		l2.TopicID("api", "l1:s:a", 1, "name"),
		l2.TopicID("api", "l1:s:a", 0, "other"),
		l2.TopicID("ap", "il1:s:a", 0, "name"), // parts cannot run into each other
	} {
		if other == base {
			t.Errorf("TopicID collided for different parts: %q", other)
		}
	}
	at := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	if l2.StanceID("t", "d", "p", at, l2.TierInferred) == l2.StanceID("t", "d", "q", at, l2.TierInferred) ||
		l2.StanceID("t", "d", "p", at, l2.TierInferred) == l2.StanceID("t", "e", "p", at, l2.TierInferred) {
		t.Error("StanceID collided for different parts")
	}
	baseStance := l2.StanceID("t", "d", "p", at, l2.TierInferred)
	if baseStance == l2.StanceID("t", "d", "p", at.Add(time.Second), l2.TierInferred) ||
		baseStance == l2.StanceID("t", "d", "p", at, l2.TierRatified) ||
		baseStance != l2.StanceID("t", "d", "p", at.In(time.FixedZone("offset", 3600)), l2.TierInferred) {
		t.Error("StanceID must distinguish versions and tiers, but not time zones")
	}
}

func validStance() l2.Stance {
	return l2.Stance{
		ID: "stance:1", TopicID: "topic:1", Position: "take the lock first",
		StatedAt: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), Evidence: []string{"l1:s:a"},
		Tier: l2.TierInferred, ACL: connector.ACL{{Kind: connector.ACLPublic}},
	}
}

func TestStanceValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*l2.Stance)
	}{
		{"no id", func(s *l2.Stance) { s.ID = "" }},
		{"no topic", func(s *l2.Stance) { s.TopicID = "" }},
		{"no position", func(s *l2.Stance) { s.Position = "" }},
		{"a position over the bound", func(s *l2.Stance) { s.Position = string(make([]byte, l2.MaxPosition+1)) }},
		{"no time", func(s *l2.Stance) { s.StatedAt = time.Time{} }},
		{"no evidence", func(s *l2.Stance) { s.Evidence = nil }},
		{"empty evidence", func(s *l2.Stance) { s.Evidence = []string{""} }},
		{"an unknown tier", func(s *l2.Stance) { s.Tier = "certain" }},
		{"no acl", func(s *l2.Stance) { s.ACL = nil }},
		{"superseding itself", func(s *l2.Stance) { s.Supersedes = s.ID }},
	}
	if err := validStance().Validate(); err != nil {
		t.Fatalf("Validate(a valid stance) = %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validStance()
			tt.change(&s)
			if err := s.Validate(); !errors.Is(err, l2.ErrInvalid) {
				t.Errorf("Validate() = %v, want l2.ErrInvalid", err)
			}
		})
	}
}

func TestScopeKeyTouches(t *testing.T) {
	tests := []struct {
		name, source, container, want string
		touches                       []connector.Touch
	}{
		{"first covering scope by id across touches", "projects", "p", "api", []connector.Touch{{Source: "github-other", Project: "acme/ops"}, {Source: "github-acme", Project: "acme/api"}}},
		{"own container wins over an earlier touches scope", "github-other", "acme/ops", "ops", []connector.Touch{{Source: "github-acme", Project: "acme/api"}}},
		{"unknown touched container", "projects", "p", "source:projects", []connector.Touch{{Source: "unknown", Project: "acme/api"}}},
		{"no touches", "projects", "p", "source:projects", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l2.ScopeKey(scopedRepo(), tt.source, tt.container, tt.touches); got != tt.want {
				t.Fatalf("ScopeKey = %q, want %q", got, tt.want)
			}
		})
	}
}

// A work source's owner source is keyed as the work source it names, so the two
// sources of a pair place their records of a project under one key whatever
// scopes cover either of them; a source that is not an owner source is keyed as
// itself.
func TestScopeKeyOfAPair(t *testing.T) {
	sources := []connector.SourceConfig{
		{ID: "projects", Type: "work"},
		{ID: "projects-owner", Type: "work", Settings: json.RawMessage(`{"owner_of":"projects"}`)},
		{ID: "other", Type: "work"},
	}
	cover := func(id string, sources ...string) config.Scope {
		s := config.Scope{ID: id}
		for _, source := range sources {
			s.Sources = append(s.Sources, config.ScopeSource{Source: source, Containers: []string{"*"}})
		}
		return s
	}
	tests := []struct {
		name   string
		scopes []config.Scope
		want   string
	}{
		{"no scope", nil, "source:projects"},
		{"a scope over both", []config.Scope{cover("work", "projects", "projects-owner")}, "work"},
		{"scopes that differ by source", []config.Scope{cover("a-owner", "projects-owner"), cover("b-work", "projects")}, "b-work"},
		{"a scope over the owner source alone", []config.Scope{cover("a-owner", "projects-owner")}, "source:projects"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := config.Repo{Sources: sources, Scopes: tt.scopes}
			work, owner := l2.ScopeKey(repo, "projects", "p1", nil), l2.ScopeKey(repo, "projects-owner", "p1", nil)
			if work != tt.want || owner != tt.want {
				t.Errorf("ScopeKey = %q for the work source and %q for the owner source, want %q for both", work, owner, tt.want)
			}
		})
	}
	if got := l2.ScopeKey(config.Repo{Sources: sources}, "other", "p1", nil); got != "source:other" {
		t.Errorf("ScopeKey(other) = %q, want its own", got)
	}
}

func TestDecisionEntityAndKey(t *testing.T) {
	d := l1.Decision{ID: "d", WorkSource: "projects", Project: "p1", Workstream: "w1"}
	if got := l2.DecisionEntity(d); got != "work:projects:p1#w1" {
		t.Errorf("DecisionEntity = %q", got)
	}
	d.Workstream = ""
	if got := l2.DecisionEntity(d); got != "work:projects:p1" {
		t.Errorf("DecisionEntity = %q", got)
	}
	first, again := l2.DecisionTopicID("l1:projects:a"), l2.DecisionTopicID("l1:projects:a")
	if first != again {
		t.Errorf("a decision topic's id must be stable: %q then %q", first, again)
	}
	if other := l2.DecisionTopicID("l1:projects-owner:a"); first == other {
		t.Errorf("distinct decision documents share the topic id %q", first)
	}
}
