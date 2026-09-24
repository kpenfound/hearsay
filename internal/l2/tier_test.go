package l2_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

func TestStand(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	days := func(n float64) time.Time { return start.Add(time.Duration(n * float64(24*time.Hour))) }
	st := func(id, doc string, at time.Time, supersedes string, j l2.Judgement) l2.Stance {
		return l2.Stance{ID: id, Evidence: []string{doc}, StatedAt: at, Supersedes: supersedes, Judgement: j}
	}
	// asserted is a stance an agent wrote through the API, citing a document.
	asserted := func(id, doc string, at time.Time, supersedes string) l2.Stance {
		s := st(id, doc, at, supersedes, "")
		s.Assertion = "evt:hearsay:assertion:" + id
		return s
	}
	evidence := map[string]l2.Evidence{
		"pr":      {Class: config.ArtifactMergedPR, Source: "github"},
		"mirror":  {Class: config.ArtifactMergedPR, Source: "mirror"},
		"open-pr": {Class: config.ArtifactPullRequest, Source: "github"},
		"issue1":  {Class: config.ArtifactIssue, Source: "github"},
		"issue2":  {Class: config.ArtifactIssue, Source: "github"},
		"issue3":  {Class: config.ArtifactIssue, Source: "github"},
		"meeting": {Class: config.ArtifactMeeting, Source: "drive"},
		"spec":    {Class: config.ArtifactSpec, Source: "drive"},
		"chat":    {Class: config.ArtifactChatThread, Source: "discord"},
	}
	def := config.DefaultPolicy()
	meetingsDecide := config.DefaultPolicy()
	meetingsDecide.Ranking = []config.ArtifactClass{config.ArtifactMeeting, config.ArtifactMergedPR, config.ArtifactSpec,
		config.ArtifactIssue, config.ArtifactPullRequest, config.ArtifactCommit, config.ArtifactChatThread, config.ArtifactDM, config.ArtifactAgent}
	specsRatify := config.DefaultPolicy()
	specsRatify.RatifiedBy.Artifacts = []config.ArtifactClass{config.ArtifactMergedPR, config.ArtifactSpec}
	githubOnly := config.DefaultPolicy()
	githubOnly.RatifiedBy.Sources = []string{"github"}
	agentsRatify := config.DefaultPolicy()
	agentsRatify.RatifiedBy.Artifacts = []config.ArtifactClass{config.ArtifactMergedPR, config.ArtifactAgent}
	agentsFromElsewhere := agentsRatify
	agentsFromElsewhere.RatifiedBy.Sources = []string{"github"}
	shortWindow := config.DefaultPolicy()
	shortWindow.ContestedWindow = 24 * time.Hour

	tests := []struct {
		name     string
		history  []l2.Stance
		policy   config.Policy
		ratified []string
		demoted  []string
		// hidden is the stances the reader may not read.
		hidden []string
		want   string
		tier   l2.Tier
	}{
		{name: "no stance", policy: def},
		{
			name:    "a lone merged pull request ratifies",
			history: []l2.Stance{st("a", "pr", days(0), "", "")},
			policy:  def, want: "a", tier: l2.TierRatified,
		},
		{
			name:    "a lone issue is inferred",
			history: []l2.Stance{st("a", "issue1", days(0), "", "")},
			policy:  def, want: "a", tier: l2.TierInferred,
		},
		{
			name:    "rank beats recency, and a newer lower-ranked change does not contest",
			history: []l2.Stance{st("a", "pr", days(0), "", ""), st("b", "chat", days(1), "a", l2.JudgementChanges)},
			policy:  def, want: "a", tier: l2.TierRatified,
		},
		{
			name:    "recency breaks a tie in rank",
			history: []l2.Stance{st("a", "issue1", days(0), "", ""), st("b", "issue2", days(1), "a", l2.JudgementRestates)},
			policy:  def, want: "b", tier: l2.TierInferred,
		},
		{
			name:    "a change between equals contests",
			history: []l2.Stance{st("a", "issue1", days(0), "", ""), st("b", "issue2", days(1), "a", l2.JudgementChanges)},
			policy:  def, want: "b", tier: l2.TierContested,
		},
		{
			name: "a late reading that changes the position the current one restates contests",
			history: []l2.Stance{
				st("a", "issue1", days(0), "", ""),
				st("late", "issue3", days(0.5), "a", l2.JudgementChanges),
				st("b", "issue2", days(1), "a", l2.JudgementRestates),
			},
			policy: def, want: "b", tier: l2.TierContested,
		},
		{
			name:    "no recorded judgement is not a disagreement",
			history: []l2.Stance{st("a", "issue1", days(0), "", ""), st("b", "issue2", days(1), "a", l2.JudgementUnknown)},
			policy:  def, want: "b", tier: l2.TierInferred,
		},
		{
			name: "a change through a restatement disagrees with what was restated",
			history: []l2.Stance{
				st("a", "issue1", days(0), "", ""),
				st("r", "chat", days(1), "a", l2.JudgementRestates),
				st("c", "issue2", days(2), "r", l2.JudgementChanges),
			},
			policy: def, want: "c", tier: l2.TierContested,
		},
		{
			name: "stances on two forks with no change between them agree",
			history: []l2.Stance{
				st("a", "issue1", days(0), "", ""),
				st("late", "issue3", days(1), "a", l2.JudgementRestates),
				st("b", "issue2", days(2), "a", l2.JudgementRestates),
			},
			policy: def, want: "b", tier: l2.TierInferred,
		},
		{
			name: "stances with no supersession between them have no recorded relation",
			history: []l2.Stance{
				st("early", "issue1", days(0), "", ""),
				st("b", "issue2", days(1), "", ""),
			},
			policy: def, want: "b", tier: l2.TierInferred,
		},
		{
			name: "an equal stance on the window's boundary is compared",
			history: []l2.Stance{
				st("a", "issue1", days(0), "", ""),
				st("b", "issue2", days(14), "a", l2.JudgementChanges),
			},
			policy: def, want: "b", tier: l2.TierContested,
		},
		{
			name: "an equal stance just outside the window is not",
			history: []l2.Stance{
				st("a", "issue1", days(0), "", ""),
				st("b", "issue2", days(14).Add(time.Nanosecond), "a", l2.JudgementChanges),
			},
			policy: def, want: "b", tier: l2.TierInferred,
		},
		{
			name: "a higher-ranked stance outside the window is no longer current",
			history: []l2.Stance{
				st("a", "pr", days(0), "", ""),
				st("b", "chat", days(15), "a", l2.JudgementChanges),
			},
			policy: def, want: "b", tier: l2.TierInferred,
		},
		{
			name: "a scope's window replaces the default",
			history: []l2.Stance{
				st("a", "issue1", days(0), "", ""),
				st("b", "issue2", days(2), "a", l2.JudgementChanges),
			},
			policy: shortWindow, want: "b", tier: l2.TierInferred,
		},
		{
			name: "a retired stance is neither current nor where the window is measured from",
			history: []l2.Stance{
				st("x", "issue1", days(0), "", ""),
				st("y2", "issue2", days(10), "y", l2.JudgementRestates),
				st("y", "issue2", days(20), "x", l2.JudgementChanges),
			},
			policy: def, want: "y2", tier: l2.TierContested,
		},
		{
			name: "a merged pull request landing settles a contested topic",
			history: []l2.Stance{
				st("a", "issue1", days(0), "", ""),
				st("b", "issue2", days(1), "a", l2.JudgementChanges),
				st("c", "pr", days(2), "b", l2.JudgementRestates),
			},
			policy: def, want: "c", tier: l2.TierRatified,
		},
		{
			name: "a scope where meetings outrank merged pull requests",
			history: []l2.Stance{
				st("a", "pr", days(0), "", ""),
				st("b", "meeting", days(1), "a", l2.JudgementChanges),
			},
			policy: meetingsDecide, want: "b", tier: l2.TierInferred,
		},
		{
			name: "the same history under the default policy",
			history: []l2.Stance{
				st("a", "pr", days(0), "", ""),
				st("b", "meeting", days(1), "a", l2.JudgementChanges),
			},
			policy: def, want: "a", tier: l2.TierRatified,
		},
		{
			name:    "an agent citing a merged pull request is an agent, and does not ratify",
			history: []l2.Stance{asserted("a", "pr", days(0), "")},
			policy:  def, want: "a", tier: l2.TierInferred,
		},
		{
			name:    "an agent ranks last: a chat thread before it stays current",
			history: []l2.Stance{st("a", "chat", days(0), "", ""), asserted("b", "pr", days(1), "a")},
			policy:  def, want: "a", tier: l2.TierInferred,
		},
		{
			name:    "an agent ratifies where the scope says agents do",
			history: []l2.Stance{asserted("a", "chat", days(0), "")},
			policy:  agentsRatify, want: "a", tier: l2.TierRatified,
		},
		{
			name:    "an agent's source is hearsay, whatever it cites",
			history: []l2.Stance{asserted("a", "pr", days(0), "")},
			policy:  agentsFromElsewhere, want: "a", tier: l2.TierInferred,
		},
		{
			name:    "a spec does not ratify by default",
			history: []l2.Stance{st("a", "spec", days(0), "", "")},
			policy:  def, want: "a", tier: l2.TierInferred,
		},
		{
			name:    "a spec ratifies where the scope says it does",
			history: []l2.Stance{st("a", "spec", days(0), "", "")},
			policy:  specsRatify, want: "a", tier: l2.TierRatified,
		},
		{
			name:    "a merged pull request from a source the scope does not accept",
			history: []l2.Stance{st("a", "mirror", days(0), "", "")},
			policy:  githubOnly, want: "a", tier: l2.TierInferred,
		},
		{
			name:    "a merged pull request from the source the scope accepts",
			history: []l2.Stance{st("a", "pr", days(0), "", "")},
			policy:  githubOnly, want: "a", tier: l2.TierRatified,
		},
		{
			name:    "an unmerged pull request does not ratify",
			history: []l2.Stance{st("a", "open-pr", days(0), "", "")},
			policy:  def, want: "a", tier: l2.TierInferred,
		},
		{
			name:    "a person ratified the current stance",
			history: []l2.Stance{st("a", "chat", days(0), "", ""), st("b", "issue1", days(1), "a", l2.JudgementChanges)},
			policy:  def, ratified: []string{"b"}, want: "b", tier: l2.TierRatified,
		},
		{
			name:    "a person ratified a stance that is not current",
			history: []l2.Stance{st("a", "chat", days(0), "", ""), st("b", "issue1", days(1), "a", l2.JudgementChanges)},
			policy:  def, ratified: []string{"a"}, want: "b", tier: l2.TierInferred,
		},
		{
			name:    "a person's ratification does not settle a contest",
			history: []l2.Stance{st("a", "issue1", days(0), "", ""), st("b", "issue2", days(1), "a", l2.JudgementChanges)},
			policy:  def, ratified: []string{"b"}, want: "b", tier: l2.TierContested,
		},
		{
			name:    "a person demoted the current stance",
			history: []l2.Stance{st("a", "issue1", days(0), "", "")},
			policy:  def, demoted: []string{"a"}, want: "a", tier: l2.TierContested,
		},
		{
			name:    "a demotion outweighs the merged pull request the stance rests on",
			history: []l2.Stance{st("a", "pr", days(0), "", "")},
			policy:  def, demoted: []string{"a"}, want: "a", tier: l2.TierContested,
		},
		{
			name:    "a newer stance the topic stands at is not contested by a demotion of the old one",
			history: []l2.Stance{st("a", "issue1", days(0), "", ""), st("b", "issue2", days(1), "a", l2.JudgementRestates)},
			policy:  def, demoted: []string{"a"}, want: "b", tier: l2.TierInferred,
		},
		{
			name:    "a demoted stance that still outranks the newer one stays current, contested",
			history: []l2.Stance{st("a", "pr", days(0), "", ""), st("b", "chat", days(1), "a", l2.JudgementRestates)},
			policy:  def, demoted: []string{"a"}, want: "a", tier: l2.TierContested,
		},
		{
			name:    "evidence that is gone ranks below every class",
			history: []l2.Stance{st("a", "chat", days(0), "", ""), st("b", "retracted", days(1), "a", l2.JudgementChanges)},
			policy:  def, want: "a", tier: l2.TierInferred,
		},
		{
			name:    "a change the reader may not read does not contest for them",
			history: []l2.Stance{st("a", "issue1", days(0), "", ""), st("b", "issue2", days(1), "a", l2.JudgementChanges)},
			policy:  def, hidden: []string{"a"}, want: "b", tier: l2.TierInferred,
		},
		{
			name: "a hidden stance hides only itself from the contest",
			history: []l2.Stance{
				st("a", "issue1", days(0), "", ""),
				st("late", "issue3", days(0.5), "a", l2.JudgementChanges),
				st("b", "issue2", days(1), "a", l2.JudgementRestates),
			},
			policy: def, hidden: []string{"a"}, want: "b", tier: l2.TierContested,
		},
		{
			name:    "a hidden current stance is still current",
			history: []l2.Stance{st("a", "chat", days(0), "", ""), st("b", "issue1", days(1), "a", l2.JudgementChanges)},
			policy:  def, hidden: []string{"b"}, want: "b", tier: l2.TierInferred,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var readable func(l2.Stance) bool
			if tt.hidden != nil {
				readable = func(st l2.Stance) bool { return !slices.Contains(tt.hidden, st.ID) }
			}
			got, ok := l2.Stand(l2.TierInputs{History: tt.history, Evidence: evidence, Policy: tt.policy, Ratified: tt.ratified, Demoted: tt.demoted, Readable: readable})
			if ok != (tt.want != "") || got.Current.ID != tt.want || got.Tier != tt.tier {
				t.Errorf("Stand() = %s at %q, %v; want %s at %q", got.Current.ID, got.Tier, ok, tt.want, tt.tier)
			}
		})
	}
}

func TestRecordedTier(t *testing.T) {
	githubOnly := config.DefaultPolicy()
	githubOnly.RatifiedBy.Sources = []string{"github"}
	doc := func(class config.ArtifactClass, source string) l1.Document {
		return l1.Document{ArtifactClass: class, Source: l1.Source{System: source}}
	}
	tests := []struct {
		name   string
		policy config.Policy
		doc    l1.Document
		want   l2.Tier
	}{
		{"a merged pull request", config.DefaultPolicy(), doc(config.ArtifactMergedPR, "github"), l2.TierRatified},
		{"an unmerged pull request", config.DefaultPolicy(), doc(config.ArtifactPullRequest, "github"), l2.TierInferred},
		{"an issue", config.DefaultPolicy(), doc(config.ArtifactIssue, "github"), l2.TierInferred},
		{"a merged pull request from a mirror", githubOnly, doc(config.ArtifactMergedPR, "mirror"), l2.TierInferred},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l2.RecordedTier(tt.policy, tt.doc); got != tt.want {
				t.Errorf("RecordedTier() = %s, want %s", got, tt.want)
			}
		})
	}
}
