package l2_test

import (
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
			if got := l2.ScopeKey(scopedRepo(), tt.source, tt.container); got != tt.want {
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

func TestTierFor(t *testing.T) {
	tests := []struct {
		kind    l1.Kind
		outcome l1.OutcomeKind
		want    l2.Tier
	}{
		{l1.KindPR, l1.OutcomeResolved, l2.TierRatified},
		{l1.KindPR, l1.OutcomeDecided, l2.TierInferred},
		{l1.KindPR, l1.OutcomeProposed, l2.TierInferred},
		{l1.KindIssue, l1.OutcomeResolved, l2.TierInferred},
		{l1.KindCommit, l1.OutcomeResolved, l2.TierInferred},
	}
	for _, tt := range tests {
		if got := l2.TierFor(doc(tt.kind, "acme/api#1", tt.outcome)); got != tt.want {
			t.Errorf("TierFor(%s, %s) = %s, want %s", tt.kind, tt.outcome, got, tt.want)
		}
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
