package bundle_test

import (
	"fmt"
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

// current is a topic standing at tier. The tier its stance was written with is
// always inferred, so a bundle that served or budgeted by the written tier
// rather than the computed one would show it.
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

// inputs is a scope with something in every section: two ratified stances and
// two inferred ones of its own and a ratified one it inherits, five recent
// documents and three open questions.
func inputs() bundle.Inputs {
	subject := doc("l1:gh:acme/api#12", l1.KindIssue, 9, "Move the lock out of the request path.\nMore detail.")
	in := bundle.Inputs{
		Scope:   "tracker:gh:acme/api#12",
		Direct:  []string{"tracker:gh:acme/api#12", "code:acme/api"},
		Subject: &subject,
		Entities: []l2.Entity{
			{ID: "code:acme/api", Type: l2.TypeProject, Name: "api", Owners: []string{"kyle"}},
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

func TestBuildDropsFromTheBottom(t *testing.T) {
	full, trimmed, _, err := bundle.Build(inputs(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if trimmed != (bundle.Trimmed{}) {
		t.Fatalf("an unlimited budget trimmed %+v", trimmed)
	}
	size := tokens(t, full)

	// Empty, not nil: nil encodes as null, which is not what Build leaves.
	withoutQuestions := full
	withoutQuestions.OpenQuestions = []bundle.Question{}
	withoutRecent := withoutQuestions
	withoutRecent.Recent.Items = []bundle.Item{}
	withoutInherited := withoutRecent
	withoutInherited.Stances = withoutRecent.Stances[:4]

	tests := []struct {
		name   string
		budget int
		want   bundle.Trimmed
		// stances are the topic names left, in order.
		stances []string
	}{
		{name: "a budget the bundle fits keeps everything", budget: size,
			stances: []string{"r1", "i1", "r2", "i2", "x1"}},
		{name: "one token over drops the last open question first", budget: size - 1,
			want: bundle.Trimmed{OpenQuestions: 1}, stances: []string{"r1", "i1", "r2", "i2", "x1"}},
		{name: "recent shrinks before stances", budget: tokens(t, withoutQuestions) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 1}, stances: []string{"r1", "i1", "r2", "i2", "x1"}},
		{name: "an inherited stance drops first, though it is ratified", budget: tokens(t, withoutRecent) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Stances: 1}, stances: []string{"r1", "i1", "r2", "i2"}},
		{name: "then the scope's own stances drop last first, the inferred ones only", budget: tokens(t, withoutInherited) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Stances: 2}, stances: []string{"r1", "i1", "r2"}},
		{name: "the scope's own ratified stances never drop, even over budget", budget: 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Stances: 3}, stances: []string{"r1", "r2"}},
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
			if n != tokens(t, got) {
				t.Errorf("reported %d tokens, the bundle is %d", n, tokens(t, got))
			}
			if tt.budget > 1 && n > tt.budget {
				t.Errorf("the bundle is %d tokens, over a budget of %d it could have met", n, tt.budget)
			}
			if len(got.Scope.Entities) != 2 || len(got.Handles) != len(bundle.Handles) {
				t.Errorf("the scope and the handles dropped: %+v %v", got.Scope, got.Handles)
			}
		})
	}
}

func TestBuildLaysOutTheDesignsShape(t *testing.T) {
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
	if got.Recent.LastActivity != "2026-09-05T19:00:00Z" || got.Recent.Items[0].When != "2026-09-05T19:00:00Z" {
		t.Errorf("recent = %+v, want absolute times", got.Recent)
	}
	if got.Stances[0].Since != "2026-09-05T18:00:00Z" {
		t.Errorf("since = %q, want an absolute time", got.Stances[0].Since)
	}
	body, err := bundle.Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{`"anchors":[]`, `"conflicts":[]`, `"handles":["get_l1","get_l0","search","stance_history","resolve"]`} {
		if !strings.Contains(string(body), section) {
			t.Errorf("the encoded bundle has no %s: %s", section, body)
		}
	}

	empty, _, _, err := bundle.Build(bundle.Inputs{Scope: "tracker:gh:acme/api#99"}, bundle.DefaultBudget)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = bundle.Encode(empty)
	if want := `{"scope":{"id":"tracker:gh:acme/api#99","entities":[]},"anchors":[],"stances":[],"recent":{"items":[]},"open_questions":[],"conflicts":[],"handles":["get_l1","get_l0","search","stance_history","resolve"]}`; string(body) != want {
		t.Errorf("an empty bundle = %s, want %s", body, want)
	}
}

func TestLine(t *testing.T) {
	long := strings.Repeat("é", bundle.MaxLine)
	tests := []struct {
		name, in, want string
	}{
		{"the first line", "one\ntwo", "one"},
		{"blank lines are skipped", "\n  \n\tthree  words  here\n", "three words here"},
		{"nothing", "", ""},
		{"a long line is cut on a character boundary", long, strings.Repeat("é", (bundle.MaxLine-len("…"))/2) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bundle.Line(tt.in)
			if got != tt.want {
				t.Errorf("Line(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if len(got) > bundle.MaxLine {
				t.Errorf("Line is %d bytes, over %d", len(got), bundle.MaxLine)
			}
		})
	}
}

// reference is Build's trim as it was first written: re-encode the whole bundle,
// drop one element, repeat. It is quadratic and obviously right, which is what a
// faster trim is checked against.
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

// mixed is inputs with stances of every kind interleaved and of uneven sizes,
// including one that escapes to more bytes than it holds.
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

// Review round 2: the faster trim keeps exactly what re-encoding after every
// drop kept — the same stances, the same counts, the same size — at every
// budget from nothing to everything.
func TestTrimMatchesReEncodingAfterEveryDrop(t *testing.T) {
	for _, n := range []int{0, 1, 2, 7, 40} {
		in := mixed(n)
		full, _, size, err := bundle.Build(in, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		// Every budget for the small cases; a stride for the large one, whose
		// reference is quadratic.
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

// Review round 2: a repository's worth of inherited ratified stances. What
// survives is the scope's own stances and the longest prefix of the inherited
// ones that fits, found here by encoding candidate bundles directly.
func TestTrimOfThousandsOfInheritedStances(t *testing.T) {
	const n = 4000
	in := inputs()
	in.Questions, in.Recent = nil, l3.Activity{}
	own := len(in.Stances) - 1 // inputs ends with one inherited stance
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
	lo, hi := 0, n // the largest m that fits, by binary search
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
	// Generous enough for -race on a slow machine; the quadratic trim took
	// seconds at half this size without it.
	if elapsed > 2*time.Second {
		t.Errorf("trimming %d stances took %s", n, elapsed)
	}
}

// The directive is a fixed block: budgeting still removes the same lower
// sections in the documented order, and the instruction itself stays verbatim.
func TestDirectiveKeepsBudgetAndOtherSections(t *testing.T) {
	in := inputs()
	in.Directive = &bundle.Directive{Text: "  @shed do this\n exactly  ", From: "kyle", Via: "discord:channel:thread", L0: "evt:1"}
	full, _, _, err := bundle.Build(in, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if full.Directive.Text != in.Directive.Text {
		t.Fatalf("directive text = %q", full.Directive.Text)
	}
	without := in
	without.Directive = nil
	base, _, _, err := bundle.Build(without, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	full.Directive = nil
	if fmt.Sprintf("%+v", full) != fmt.Sprintf("%+v", base) {
		t.Fatal("a directive changed another section")
	}
	budget := tokens(t, base)
	with, trimmed, used, err := bundle.Build(in, budget)
	if err != nil {
		t.Fatal(err)
	}
	if with.Directive == nil || trimmed.OpenQuestions == 0 || used > budget || tokens(t, with) != used {
		t.Fatalf("budgeted bundle: directive=%+v trimmed=%+v used=%d budget=%d", with.Directive, trimmed, used, budget)
	}
}
