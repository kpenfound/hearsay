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

func current(topic string, tier l2.Tier, hour int) l3.CurrentStance {
	return l3.CurrentStance{
		Topic: l2.Topic{ID: "topic:" + topic, Name: topic},
		Stance: l2.Stance{
			ID: "stance:" + topic, Position: "the position on " + topic, Tier: tier,
			StatedAt: day.Add(time.Duration(hour) * time.Hour), Evidence: []string{"l1:gh:acme/api#" + topic},
		},
	}
}

// inputs is a scope with something in every section: two ratified stances and
// two inferred ones, five recent documents and three open questions.
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

	tests := []struct {
		name   string
		budget int
		want   bundle.Trimmed
		// stances are the topic names left, in order.
		stances []string
	}{
		{name: "a budget the bundle fits keeps everything", budget: size,
			stances: []string{"r1", "i1", "r2", "i2"}},
		{name: "one token over drops the last open question first", budget: size - 1,
			want: bundle.Trimmed{OpenQuestions: 1}, stances: []string{"r1", "i1", "r2", "i2"}},
		{name: "recent shrinks before stances", budget: tokens(t, withoutQuestions) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 1}, stances: []string{"r1", "i1", "r2", "i2"}},
		{name: "stances drop last first, the inferred ones only", budget: tokens(t, withoutRecent) - 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Stances: 1}, stances: []string{"r1", "i1", "r2"}},
		{name: "ratified stances never drop, even over budget", budget: 1,
			want: bundle.Trimmed{OpenQuestions: 3, Recent: 5, Stances: 2}, stances: []string{"r1", "r2"}},
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
