package config_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
)

// The design doc fixes four positions in the default ranking, and everything
// else about authority is built on them holding.
func TestDefaultPolicyRanksTheWayTheDesignSays(t *testing.T) {
	p := config.DefaultPolicy()
	order := []config.ArtifactClass{
		config.ArtifactMergedPR,
		config.ArtifactMeeting,
		config.ArtifactChatThread,
		config.ArtifactDM,
	}
	for i := range len(order) - 1 {
		if !p.Outranks(order[i], order[i+1]) {
			t.Errorf("%s does not outrank %s", order[i], order[i+1])
		}
		if p.Outranks(order[i+1], order[i]) {
			t.Errorf("%s outranks %s, and the design doc says the other way round", order[i+1], order[i])
		}
	}
	// An agent's own words rank below every human artifact.
	for _, c := range config.ArtifactClasses() {
		if c != config.ArtifactAgent && !p.Outranks(c, config.ArtifactAgent) {
			t.Errorf("%s does not outrank %s", c, config.ArtifactAgent)
		}
	}
	// Every class is ranked, or a stance from it would be unrankable.
	for _, c := range config.ArtifactClasses() {
		if _, ok := p.Rank(c); !ok {
			t.Errorf("the default ranking does not rank %s", c)
		}
	}
	if !p.RatifiedByArtifact(config.ArtifactMergedPR, "github") {
		t.Error("a merged pull request does not ratify by default")
	}
	if p.RatifiedByArtifact(config.ArtifactMeeting, "drive") {
		t.Error("a meeting ratifies by default, and only a merged PR should")
	}
	if !p.RatifiedByPrincipal("anyone-at-all") {
		t.Error("nobody may ratify by default, and by default anyone may")
	}
}

// A class nobody ranked is below everything that is ranked, and level with the
// other unranked ones, so that leaving a class out of an override is not the
// same as putting it last.
func TestRankOfAnUnrankedClass(t *testing.T) {
	p := config.Policy{Ranking: []config.ArtifactClass{config.ArtifactMeeting, config.ArtifactDM}}

	rank, ok := p.Rank(config.ArtifactCommit)
	if ok || rank != 0 {
		t.Errorf("Rank(commit) = %d, %v, want 0, false", rank, ok)
	}
	if rank, ok := p.Rank(config.ArtifactMeeting); !ok || rank <= 0 {
		t.Errorf("Rank(meeting) = %d, %v, want a positive rank", rank, ok)
	}
	if !p.Outranks(config.ArtifactDM, config.ArtifactCommit) {
		t.Error("a ranked class does not outrank an unranked one")
	}
	if p.Outranks(config.ArtifactCommit, config.ArtifactSpec) {
		t.Error("one unranked class outranks another; they should tie so time decides")
	}
	if p.Outranks(config.ArtifactMeeting, config.ArtifactMeeting) {
		t.Error("a class outranks itself")
	}
}

func TestRatifiedByArtifactIsRestrictedBySource(t *testing.T) {
	p := config.Policy{
		RatifiedBy: config.Ratifiers{
			Sources:   []string{"github"},
			Artifacts: []config.ArtifactClass{config.ArtifactMergedPR},
		},
	}
	tests := []struct {
		class  config.ArtifactClass
		source string
		want   bool
	}{
		{config.ArtifactMergedPR, "github", true},
		{config.ArtifactMergedPR, "github-mirror", false},
		{config.ArtifactMeeting, "github", false},
		{config.ArtifactMergedPR, "", false},
	}
	for _, tt := range tests {
		if got := p.RatifiedByArtifact(tt.class, tt.source); got != tt.want {
			t.Errorf("RatifiedByArtifact(%q, %q) = %v, want %v", tt.class, tt.source, got, tt.want)
		}
	}
	// The source list restricts artifacts, not people: a person ratifying is a
	// person, not a source.
	anyone := config.Policy{RatifiedBy: config.Ratifiers{Principals: []string{"kyle"}, Sources: []string{"github"}}}
	if !anyone.RatifiedByPrincipal("kyle") {
		t.Error("a listed principal may not ratify when sources are restricted")
	}
	if anyone.RatifiedByPrincipal("") {
		t.Error("an empty principal id ratifies")
	}
}

// A scope inherits every field it does not set, and replaces the ones it does.
// Nothing is appended to an inherited list.
func TestAuthorityComposition(t *testing.T) {
	tests := []struct {
		name      string
		authority string // the authority/ file
		scope     string
		want      config.Policy
	}{
		{
			name:  "no authority configured at all",
			scope: "api",
			want:  config.DefaultPolicy(),
		},
		{
			name:      "the default policy is overridden for every scope",
			authority: "scope: \"*\"\nratified_by:\n  principals: [kyle]\n",
			scope:     "web",
			want: config.Policy{
				Scope:   "*",
				Ranking: config.DefaultPolicy().Ranking,
				RatifiedBy: config.Ratifiers{
					Principals: []string{"kyle"},
					Sources:    []string{"*"},
					Artifacts:  []config.ArtifactClass{config.ArtifactMergedPR},
				},
			},
		},
		{
			name: "a scope replaces one field and inherits the rest",
			authority: "- scope: \"*\"\n  ratified_by:\n    principals: [kyle]\n" +
				"- scope: api\n  ranking: [meeting, merged_pr]\n",
			scope: "api",
			want: config.Policy{
				Scope:   "api",
				Ranking: []config.ArtifactClass{config.ArtifactMeeting, config.ArtifactMergedPR},
				RatifiedBy: config.Ratifiers{
					Principals: []string{"kyle"},
					Sources:    []string{"*"},
					Artifacts:  []config.ArtifactClass{config.ArtifactMergedPR},
				},
			},
		},
		{
			name: "an empty list inherits nothing",
			authority: "- scope: \"*\"\n  ratified_by:\n    principals: [kyle]\n" +
				"- scope: api\n  ratified_by:\n    principals: []\n",
			scope: "api",
			want: config.Policy{
				Scope:   "api",
				Ranking: config.DefaultPolicy().Ranking,
				RatifiedBy: config.Ratifiers{
					Principals: []string{},
					Sources:    []string{"*"},
					Artifacts:  []config.ArtifactClass{config.ArtifactMergedPR},
				},
			},
		},
		{
			name:      "a scope with no policy of its own gets the default",
			authority: "- scope: api\n  ranking: [meeting]\n",
			scope:     "web",
			want:      config.DefaultPolicy(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := with(map[string]string{
				"principals/p.yaml": "id: kyle\nidentities: [{source: github, handle: kpenfound}]\n",
				"scopes/api.yaml":   "- id: api\n  sources: [github]\n- id: web\n  sources: [github]\n",
			})
			if tt.authority != "" {
				files["authority/a.yaml"] = tt.authority
			}
			repo, err := config.Load(writeFiles(t, files))
			if err != nil {
				t.Fatalf("Load() = %v, want no error", err)
			}

			got := repo.Authority.ForScope(tt.scope)
			if !slices.Equal(got.Ranking, tt.want.Ranking) {
				t.Errorf("ranking = %v, want %v", got.Ranking, tt.want.Ranking)
			}
			if !slices.Equal(got.RatifiedBy.Principals, tt.want.RatifiedBy.Principals) {
				t.Errorf("ratified_by.principals = %#v, want %#v", got.RatifiedBy.Principals, tt.want.RatifiedBy.Principals)
			}
			if !slices.Equal(got.RatifiedBy.Sources, tt.want.RatifiedBy.Sources) {
				t.Errorf("ratified_by.sources = %v, want %v", got.RatifiedBy.Sources, tt.want.RatifiedBy.Sources)
			}
			if !slices.Equal(got.RatifiedBy.Artifacts, tt.want.RatifiedBy.Artifacts) {
				t.Errorf("ratified_by.artifacts = %v, want %v", got.RatifiedBy.Artifacts, tt.want.RatifiedBy.Artifacts)
			}
		})
	}
}

// An empty list and an absent one mean different things, and a policy that
// empties one has to keep it empty rather than inheriting it back.
func TestEmptyRatifierListLocksRatificationOut(t *testing.T) {
	repo, err := config.Load(writeFiles(t, with(map[string]string{
		"principals/p.yaml": "id: kyle\nidentities: [{source: github, handle: kpenfound}]\n",
		"authority/a.yaml": "- scope: \"*\"\n  ratified_by:\n    principals: [kyle]\n" +
			"- scope: api\n  ratified_by:\n    principals: []\n    artifacts: []\n",
	})))
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}

	api := repo.Authority.ForScope("api")
	if api.RatifiedByPrincipal("kyle") {
		t.Error("kyle may ratify in a scope whose ratifiers are empty")
	}
	if api.RatifiedByArtifact(config.ArtifactMergedPR, "github") {
		t.Error("a merged PR ratifies in a scope whose ratifying artifacts are empty")
	}
	if !repo.Authority.Default().RatifiedByPrincipal("kyle") {
		t.Error("emptying one scope's ratifiers changed the default")
	}
}

// The zero Authority is a process with no authority/ directory, and it still
// has to answer every question asked of it.
func TestZeroAuthorityIsTheDefaultPolicy(t *testing.T) {
	var a config.Authority
	if got := a.ForScope("anything"); !slices.Equal(got.Ranking, config.DefaultPolicy().Ranking) {
		t.Errorf("the zero Authority ranks %v, want the default %v", got.Ranking, config.DefaultPolicy().Ranking)
	}
	if !a.Default().RatifiedByArtifact(config.ArtifactMergedPR, "github") {
		t.Error("the zero Authority does not ratify a merged pull request")
	}
	if len(a.Scopes()) != 0 {
		t.Errorf("the zero Authority has scopes %v", a.Scopes())
	}
}

// The classes are a closed set: an authority policy naming something outside it
// would be a typo that silently ranks nothing.
func TestArtifactClassesAreClosed(t *testing.T) {
	for _, c := range config.ArtifactClasses() {
		if !c.Valid() {
			t.Errorf("%q is listed by ArtifactClasses but is not valid", c)
		}
	}
	for _, c := range []config.ArtifactClass{"", "standup", "MERGED_PR", "pr"} {
		if c.Valid() {
			t.Errorf("%q is valid, want it rejected", c)
		}
	}
}
