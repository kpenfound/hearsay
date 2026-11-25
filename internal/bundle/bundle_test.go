package bundle_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
)

var day = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

func doc(id string, kind l1.Kind, hour int, summary string) l1.Stored {
	return l1.Stored{Document: l1.Document{
		ID: id, Kind: kind,
		Time: l1.Times{LastActivity: day.Add(time.Duration(hour) * time.Hour)},
		Body: l1.Body{Summary: summary},
	}}
}

func current(topic string, tier l2.Tier, hour int) l3.CurrentStance {
	return l3.CurrentStance{
		Topic: l2.Topic{ID: "topic:" + topic, Name: topic},
		Stance: l2.Stance{
			ID: "stance:" + topic, Position: "the position on " + topic, Tier: l2.TierInferred,
			StatedAt: day.Add(time.Duration(hour) * time.Hour), Evidence: []string{"l1:gh:acme/api#" + topic},
		},
		Tier: tier,
	}
}

func inherited(c l3.CurrentStance) l3.CurrentStance {
	c.Inherited = true
	return c
}

func inputs() bundle.Inputs {
	subject := doc("l1:gh:acme/api#12", l1.KindIssue, 9, "Move the lock out of the request path.\nMore detail.")
	in := bundle.Inputs{
		Scope:   "tracker:gh:acme/api#12",
		Direct:  []string{"tracker:gh:acme/api#12", "code:acme/api"},
		Subject: &subject,
		Entities: []l2.Entity{
			{ID: "code:acme/api", Type: l2.TypeProject, Name: "api", Owners: []string{"kyle"}},
		},
		Anchors: []l3.Anchor{
			{Doc: doc("l1:gh:acme/api#1200", l1.KindIssue, 1, "Engine schema v3 design.\nThe long version."), PinnedBy: "kyle"},
			{Doc: doc("l1:wiki:design/locks.md#locking", l1.KindWikiSection, 2, "How the request path takes its locks.")},
		},
		Stances: []l3.CurrentStance{
			current("r1", l2.TierRatified, 8),
			current("i1", l2.TierInferred, 7),
			current("r2", l2.TierRatified, 6),
			current("i2", l2.TierInferred, 5),
			inherited(current("x1", l2.TierRatified, 9)),
		},
		Recent: l3.Activity{LastActivity: day.Add(9 * time.Hour)},
		Questions: []l3.Question{
			{Text: "q1", Evidence: []string{"l1:gh:acme/api#12"}},
			{Text: "q2", Evidence: []string{"l1:gh:acme/api#12"}},
			{Text: "q3", Evidence: []string{"l1:gh:acme/api#13"}},
		},
	}
	for i := range 5 {
		in.Recent.Items = append(in.Recent.Items, doc(fmt.Sprintf("l1:gh:acme/api#%d", 20+i), l1.KindPR, 9-i, "recent work"))
	}
	return in
}

func tokens(t *testing.T, b bundle.Bundle) int {
	t.Helper()
	body, err := bundle.Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	return bundle.Tokens(body)
}

func TestDirectiveConflicts(t *testing.T) {
	base := func() bundle.Inputs {
		return bundle.Inputs{Scope: "tracker:gh:acme/api#12", Stances: []l3.CurrentStance{
			current("beta", l2.TierInferred, 2), current("alpha", l2.TierRatified, 1),
			current("delta", l2.TierContested, 3), current("gamma", l2.TierRatified, 4),
		}}
	}
	tests := []struct {
		name    string
		setup   func(*bundle.Inputs)
		budget  int
		want    []string
		trimmed bool
	}{
		{"no directive", func(*bundle.Inputs) {}, 10000, nil, false},
		{"entity reference", func(in *bundle.Inputs) {
			in.Directive = &bundle.Directive{Text: "do something", References: []l1.Reference{{Type: l1.RefSystem, ID: "code:engine"}}}
			in.Stances[0].Topic.About = []string{"code:engine"}
		}, 10000, []string{"topic:beta"}, false},
		{"evidence reference", func(in *bundle.Inputs) {
			in.Directive = &bundle.Directive{Text: "see acme/api#alpha", References: []l1.Reference{{Type: l1.RefIssue, ID: "acme/api#alpha"}}}
		}, 10000, []string{"topic:alpha"}, false},
		{"document URL reference", func(in *bundle.Inputs) {
			in.Directive = &bundle.Directive{Text: "see the document", References: []l1.Reference{{Type: l1.RefURL, ID: "https://example.test/decision"}}}
			in.EvidenceURLs = map[string]string{"l1:gh:acme/api#alpha": "https://example.test/decision"}
		}, 10000, []string{"topic:alpha"}, false},
		{"text match", func(in *bundle.Inputs) {
			in.Directive = &bundle.Directive{Text: "Please review the position on beta before merging"}
		}, 10000, []string{"topic:beta"}, false},
		{"budget trimmed stance", func(in *bundle.Inputs) {
			in.Directive = &bundle.Directive{Text: "the position on beta"}
		}, 1, []string{"topic:beta"}, true},
		{"cap and tier order", func(in *bundle.Inputs) {
			in.Directive = &bundle.Directive{Text: "all", References: []l1.Reference{{Type: l1.RefSystem, ID: "code:engine"}}}
			for i := range in.Stances {
				in.Stances[i].Topic.About = []string{"code:engine"}
			}
		}, 10000, []string{"topic:alpha", "topic:gamma", "topic:delta"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			tt.setup(&in)
			b, trimmed, _, err := bundle.Build(in, tt.budget)
			if err != nil {
				t.Fatal(err)
			}
			if tt.trimmed && trimmed.Stances == 0 {
				t.Fatal("expected stance trim")
			}
			got := []string{}
			for _, c := range b.Conflicts {
				got = append(got, c.TopicID)
				if c.Current == "" || c.Tier == "" || c.Stakes == "" {
					t.Errorf("incomplete conflict: %+v", c)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("conflicts = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildDropsFromTheBottom(t *testing.T) {
	full, trimmed, _, err := bundle.Build(inputs(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if trimmed != (bundle.Trimmed{}) {
		t.Fatalf("an unlimited budget trimmed %+v", trimmed)
	}
	size := tokens(t, full)

	withoutQuestions := full
	withoutQuestions.OpenQuestions = []bundle.Question{}
	withoutRecent := withoutQuestions
	withoutRecent.Recent.Items = []bundle.Item{}
	withoutAnchors := withoutRecent
	withoutAnchors.Anchors = []bundle.Anchor{}
	withoutInherited := withoutAnchors
	withoutInherited.Stances = withoutAnchors.Stances[:4]

	tests := []struct {
		name    string
		budget  int
		want    bundle.Trimmed
		anchors int
		stances []string
	}{
		{name: "a budget the bundle fits keeps everything", budget: size,
			anchors: 2, stances: []string{"r1", "i1", "r2", "i2", "x1"}},
		{name: "one token over drops the last open question first", budget: size - 1,
			want: bundle.Trimmed{OpenQuestions: 1}, anchors: 2, stances: []string{"r1", "i1", "r2", "i2", "x1"}},
		{name: "recent shrinks before anchors and stances", budget: tokens(t, withoutQuestions) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 1}, anchors: 2, stances: []string{"r1", "i1", "r2", "i2", "x1"}},
		{name: "anchors drop after recent, last first, before any stance", budget: tokens(t, withoutRecent) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Anchors: 1}, anchors: 1, stances: []string{"r1", "i1", "r2", "i2", "x1"}},
		{name: "an inherited stance drops first, though it is ratified", budget: tokens(t, withoutAnchors) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Anchors: 2, Stances: 1}, stances: []string{"r1", "i1", "r2", "i2"}},
		{name: "then the scope's own stances drop last first, the inferred ones only", budget: tokens(t, withoutInherited) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Anchors: 2, Stances: 2}, stances: []string{"r1", "i1", "r2"}},
		{name: "the scope's own ratified stances never drop, even over budget", budget: 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Anchors: 2, Stances: 3}, stances: []string{"r1", "r2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, trimmed, n, err := bundle.Build(inputs(), tt.budget)
			if err != nil {
				t.Fatal(err)
			}
			if trimmed != tt.want {
				t.Errorf("trimmed = %+v, want %+v", trimmed, tt.want)
			}
			var names []string
			for _, s := range got.Stances {
				names = append(names, s.Topic)
			}
			if strings.Join(names, ",") != strings.Join(tt.stances, ",") {
				t.Errorf("stances = %v, want %v", names, tt.stances)
			}
			if len(got.Anchors) != tt.anchors || (tt.anchors > 0 && got.Anchors[0].PinnedBy != "kyle") {
				t.Errorf("anchors = %+v, want the first %d", got.Anchors, tt.anchors)
			}
			if n != tokens(t, got) {
				t.Errorf("reported %d tokens, the bundle is %d", n, tokens(t, got))
			}
			if tt.budget > 1 && n > tt.budget {
				t.Errorf("the bundle is %d tokens, over a budget of %d it could have met", n, tt.budget)
			}
		})
	}
}

func TestBuildScopeEntitiesCarryTheirDocumentLine(t *testing.T) {
	got, _, _, err := bundle.Build(inputs(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	subject := got.Scope.Entities[0]
	if subject.L1 != "l1:gh:acme/api#12" || subject.Line != "Move the lock out of the request path." {
		t.Errorf("the scope's own entity = %+v, want its document's id and the first line of its summary", subject)
	}
	if code := got.Scope.Entities[1]; code.L1 != "" || code.Line != "" || code.Name != "api" || code.Owners[0] != "kyle" {
		t.Errorf("a code entity = %+v, want its name and owners and no line", code)
	}
}

func TestBuildTimesAreAbsolute(t *testing.T) {
	got, _, _, err := bundle.Build(inputs(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.Recent.LastActivity != "2026-09-05T19:00:00Z" || got.Recent.Items[0].When != "2026-09-05T19:00:00Z" {
		t.Errorf("recent = %+v, want absolute times", got.Recent)
	}
	if got.Stances[0].Since != "2026-09-05T18:00:00Z" {
		t.Errorf("since = %q, want an absolute time", got.Stances[0].Since)
	}
}

func TestEncodeSectionsAreTheDesignsShape(t *testing.T) {
	got, _, _, err := bundle.Build(inputs(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	body, err := bundle.Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	anchors := `"anchors":[{"l1":"l1:gh:acme/api#1200","kind":"issue","line":"Engine schema v3 design.","pinned_by":"kyle"},` +
		`{"l1":"l1:wiki:design/locks.md#locking","kind":"wiki_section","line":"How the request path takes its locks."}]`
	for _, section := range []string{anchors, `"conflicts":[]`, `"handles":["get_l1","get_l0","search","stance_history","resolve","traverse"]`} {
		if !strings.Contains(string(body), section) {
			t.Errorf("the encoded bundle has no %s: %s", section, body)
		}
	}
}

func TestEncodeEmptyBundle(t *testing.T) {
	empty, _, _, err := bundle.Build(bundle.Inputs{Scope: "tracker:gh:acme/api#99"}, bundle.DefaultBudget)
	if err != nil {
		t.Fatal(err)
	}
	body, err := bundle.Encode(empty)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"scope":{"id":"tracker:gh:acme/api#99","entities":[]},"anchors":[],"stances":[],"recent":{"items":[]},"open_questions":[],"conflicts":[],"handles":["get_l1","get_l0","search","stance_history","resolve","traverse"]}`
	if string(body) != want {
		t.Errorf("an empty bundle = %s, want %s", body, want)
	}
}

func TestBuildKeepsScopeAndHandlesAtAnyBudget(t *testing.T) {
	for _, budget := range []int{0, 1, 1 << 20} {
		got, _, _, err := bundle.Build(inputs(), budget)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Scope.Entities) != 2 || !slices.Equal(got.Handles, bundle.Handles) {
			t.Errorf("budget %d: the scope and the handles dropped: %+v %v", budget, got.Scope, got.Handles)
		}
	}
}

func reference(b bundle.Bundle, budget int) (bundle.Bundle, bundle.Trimmed, int) {
	var trimmed bundle.Trimmed
	for {
		body, _ := bundle.Encode(b)
		tokens := bundle.Tokens(body)
		if tokens <= budget {
			return b, trimmed, tokens
		}
		switch {
		case len(b.OpenQuestions) > 0:
			b.OpenQuestions = b.OpenQuestions[:len(b.OpenQuestions)-1]
			trimmed.OpenQuestions++
		case len(b.Recent.Items) > 0:
			b.Recent.Items = b.Recent.Items[:len(b.Recent.Items)-1]
			trimmed.Recent++
		case len(b.Anchors) > 0:
			b.Anchors = b.Anchors[:len(b.Anchors)-1]
			trimmed.Anchors++
		default:
			last := -1
			for i := len(b.Stances) - 1; i >= 0 && last < 0; i-- {
				if b.Stances[i].Inherited {
					last = i
				}
			}
			for i := len(b.Stances) - 1; i >= 0 && last < 0; i-- {
				if b.Stances[i].Tier != string(l2.TierRatified) {
					last = i
				}
			}
			if last < 0 {
				return b, trimmed, tokens
			}
			b.Stances = append(b.Stances[:last:last], b.Stances[last+1:]...)
			trimmed.Stances++
		}
	}
}

func mixed(n int) bundle.Inputs {
	in := inputs()
	in.Stances = nil
	tiers := []l2.Tier{l2.TierRatified, l2.TierInferred, l2.TierContested}
	for i := range n {
		c := current(fmt.Sprintf("t%d <&> %s", i, strings.Repeat("x", i%37)), tiers[i%3], i%11)
		if i%4 == 1 {
			c = inherited(c)
		}
		in.Stances = append(in.Stances, c)
	}
	return in
}

func TestTrimMatchesReEncodingAfterEveryDrop(t *testing.T) {
	for _, n := range []int{0, 1, 2, 7, 40} {
		in := mixed(n)
		full, _, size, err := bundle.Build(in, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		stride := 1 + n/5
		for budget := 0; budget <= size+1; budget += stride {
			got, trimmed, tokens, err := bundle.Build(in, budget)
			if err != nil {
				t.Fatalf("n=%d budget=%d: %v", n, budget, err)
			}
			want, wantTrimmed, wantTokens := reference(full, budget)
			gotBody, _ := bundle.Encode(got)
			wantBody, _ := bundle.Encode(want)
			if string(gotBody) != string(wantBody) || trimmed != wantTrimmed || tokens != wantTokens {
				t.Fatalf("n=%d budget=%d: got %+v %d tokens, want %+v %d tokens\n got %s\nwant %s",
					n, budget, trimmed, tokens, wantTrimmed, wantTokens, gotBody, wantBody)
			}
		}
	}
}

func TestTrimOfThousandsOfInheritedStances(t *testing.T) {
	const n = 4000
	in := inputs()
	in.Questions, in.Recent, in.Anchors = nil, l3.Activity{}, nil
	own := len(in.Stances) - 1
	in.Stances = in.Stances[:own]
	for i := range n {
		in.Stances = append(in.Stances, inherited(current(fmt.Sprintf("inherited %04d %s", i, strings.Repeat("y", 160)), l2.TierRatified, 3)))
	}
	full, _, _, err := bundle.Build(in, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	fits := func(m int) bool {
		b := full
		b.Stances = full.Stances[:own+m]
		return tokens(t, b) <= bundle.DefaultBudget
	}
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	start := time.Now()
	got, trimmed, size, err := bundle.Build(in, bundle.DefaultBudget)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if len(got.Stances) != own+lo || trimmed != (bundle.Trimmed{Stances: n - lo}) || size != tokens(t, got) || size > bundle.DefaultBudget {
		t.Errorf("kept %d stances, trimmed %+v, %d tokens; want %d kept and %d trimmed within %d",
			len(got.Stances), trimmed, size, own+lo, n-lo, bundle.DefaultBudget)
	}
	for i := range own {
		if got.Stances[i].Inherited {
			t.Fatalf("stance %d is inherited; the scope's own come first and stay", i)
		}
	}
	if elapsed > 2*time.Second {
		t.Errorf("trimming %d stances took %s", n, elapsed)
	}
}

func directiveInputs() bundle.Inputs {
	in := inputs()
	in.Directive = &bundle.Directive{Text: "  @shed do this\n exactly  ", From: "kyle", Via: "discord:channel:thread", L0: "evt:1"}
	return in
}

func TestDirectiveTextIsServedVerbatim(t *testing.T) {
	in := directiveInputs()
	got, _, _, err := bundle.Build(in, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.Directive.Text != in.Directive.Text {
		t.Fatalf("directive text = %q, want %q", got.Directive.Text, in.Directive.Text)
	}
}

func TestDirectiveChangesNoOtherSection(t *testing.T) {
	in := directiveInputs()
	with, _, _, err := bundle.Build(in, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	in.Directive = nil
	base, _, _, err := bundle.Build(in, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	with.Directive = nil
	if fmt.Sprintf("%+v", with) != fmt.Sprintf("%+v", base) {
		t.Fatal("a directive changed another section")
	}
}

func TestDirectiveIsHeldToTheBudgetByTrimmingLowerSections(t *testing.T) {
	in := directiveInputs()
	without := in
	without.Directive = nil
	base, _, _, err := bundle.Build(without, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	budget := tokens(t, base)
	got, trimmed, used, err := bundle.Build(in, budget)
	if err != nil {
		t.Fatal(err)
	}
	if got.Directive == nil || trimmed.OpenQuestions == 0 || used > budget || tokens(t, got) != used {
		t.Fatalf("budgeted bundle: directive=%+v trimmed=%+v used=%d budget=%d", got.Directive, trimmed, used, budget)
	}
}
