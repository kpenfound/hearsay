package eval_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/llm"
)

func shortlistSeed(t *testing.T) (eval.Corpus, eval.Predictions, eval.QualificationOptions) {
	t.Helper()
	var c eval.Corpus
	var p eval.Predictions
	readFixture(t, "testdata/seed-corpus.json", &c)
	readFixture(t, "testdata/seed-predictions.json", &p)
	return c, p, eval.QualificationOptions{Task: llm.TaskTopicShortlist, Name: "test-only", ModelVersion: "fixture-judge-v1", Limits: map[string]float64{"errors": 0, "shortlist_omissions": 0}, MinimumPerClass: map[string]int{"existing": 1, "none_fit": 1}}
}

func TestShortlistQualificationScoresTerminalNoneFit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		premature bool
		status    string
	}{
		{"complete selection", false, "qualified"},
		{"premature stop", true, "failed_qualification"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, o := shortlistSeed(t)
			last := slices.IndexFunc(c.Examples, func(e eval.Example) bool { return e.ID == "held_out-shortlist-premature" })
			if !tc.premature {
				p.Jev[last].Shortlist = c.Examples[last].Shortlist
			}
			q, err := eval.Qualify(c, p, o)
			if err != nil || q.Status != tc.status {
				t.Fatalf("qualification=%+v %v", q, err)
			}
			if q.Threshold != .95 || q.AdoptedPerClass["none_fit"] != 2 || q.AdoptedPerClass["existing"] != 2 {
				t.Fatalf("terminal stops not counted: %+v", q)
			}
			if tc.premature {
				if len(q.Policies) != 0 || !reflect.DeepEqual(q.FailedLimits, []string{"errors", "shortlist_omissions"}) {
					t.Fatalf("failed=%+v", q)
				}
				assertMeasure(t, q.HeldOut.Errors, 1, 4)
				assertMeasure(t, q.HeldOut.ShortlistOmissions, 4, 12)
			} else {
				assertConfigPolicy(t, q)
				assertMeasure(t, q.HeldOut.Errors, 0, 4)
			}
		})
	}
}

func TestShortlistAdoptionRequiresEntireNonemptyQualifiedSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*eval.Prediction)
		adopted int
	}{
		{"correct stop", func(*eval.Prediction) {}, 1},
		{"low terminal confidence", func(p *eval.Prediction) { p.Confidence = share(.2) }, 0},
		{"low prefix confidence", func(p *eval.Prediction) { p.Confidence = share(.1) }, 0},
		{"empty prefix stop", func(p *eval.Prediction) { p.Shortlist = &eval.Shortlist{Selected: []string{}, End: "none_fit"} }, 0},
		{"unusable slot", func(p *eval.Prediction) { p.Shortlist = nil; p.Label = "abstain"; p.Confidence = nil }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, o := shortlistSeed(t)
			// Keep one nonempty stop in each split; the separate held-out
			// premature-stop fixture is exercised by the refusal test above.
			keep := func(id string) bool {
				return id != "held_out-shortlist-premature" && id != "held_out-shortlist-correct-stop"
			}
			c.Examples = slices.DeleteFunc(c.Examples, func(e eval.Example) bool { return !keep(e.ID) })
			p.Jev = slices.DeleteFunc(p.Jev, func(p eval.Prediction) bool { return !keep(p.ExampleID) })
			p.Baseline = slices.DeleteFunc(p.Baseline, func(p eval.Prediction) bool { return !keep(p.ExampleID) })
			for i, e := range c.Examples {
				if e.ID == "held_out-08" {
					tc.change(&p.Jev[i])
				}
			}
			q, err := eval.Qualify(c, p, o)
			if err != nil {
				t.Fatal(err)
			}
			if q.AdoptedPerClass["none_fit"] != tc.adopted {
				t.Fatalf("q=%+v", q)
			}
			if tc.adopted == 0 && (q.Status != "insufficient_evidence" || !slices.Contains(q.InsufficientClasses, "adopted:none_fit")) {
				t.Fatalf("q=%+v", q)
			}
		})
	}
}

func TestShortlistCorpusRequiresCompleteSelection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*eval.Example)
	}{
		{"single slot label", func(e *eval.Example) { e.Shortlist = nil; e.Label = "topic-1" }},
		{"unknown candidate", func(e *eval.Example) { e.Shortlist = &eval.Shortlist{Selected: []string{"hidden"}, End: "none_fit"} }},
		{"duplicate selection", func(e *eval.Example) {
			e.Shortlist = &eval.Shortlist{Selected: []string{"topic-1", "topic-1"}, End: "none_fit"}
		}},
		{"missing end", func(e *eval.Example) { e.Shortlist.End = "" }},
		{"premature capacity", func(e *eval.Example) { e.Shortlist = &eval.Shortlist{Selected: []string{"topic-1"}, End: "capacity"} }},
		{"premature pool end", func(e *eval.Example) {
			e.Shortlist = &eval.Shortlist{Selected: []string{"topic-1"}, End: "pool_exhausted"}
		}},
		{"stop after capacity", func(e *eval.Example) { e.Shortlist.End = "none_fit" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := shortlistSeed(t)
			tc.mutate(&c.Examples[6])
			if err := c.Validate(); err == nil {
				t.Fatal("incomplete shortlist accepted")
			}
		})
	}
}

func TestShortlistSetErrors(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		gold, pred                  []string
		end                         string
		errors, omitted, irrelevant int
	}{
		{"premature none fit", []string{"a", "b"}, []string{"a"}, "none_fit", 1, 1, 0},
		{"wrong candidate", []string{"a", "b"}, []string{"a", "c"}, "none_fit", 1, 1, 1},
		{"same set in prompt order", []string{"a", "b"}, []string{"b", "a"}, "none_fit", 0, 0, 0},
		{"exhausted pool", []string{"a", "b", "c"}, []string{"c", "a", "b"}, "pool_exhausted", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := example("shortlist", llm.TaskTopicShortlist, "", "held_out")
			e.Candidates = []string{"a", "b", "c"}
			e.Shortlist = &eval.Shortlist{Selected: tc.gold, End: tc.end}
			p := eval.Prediction{ExampleID: e.ID, Shortlist: &eval.Shortlist{Selected: tc.pred, End: tc.end}}
			r, err := eval.CompareJudgments(corpus(e), eval.Predictions{Jev: []eval.Prediction{p}}, "")
			if err != nil {
				t.Fatal(err)
			}
			m := r[llm.TaskTopicShortlist].Jev
			assertMeasure(t, m.Errors, tc.errors, 1)
			assertMeasure(t, m.ShortlistOmissions, tc.omitted, len(tc.gold))
			assertMeasure(t, m.IncorrectAttachments, tc.irrelevant, len(tc.pred))
			assertMeasure(t, m.DuplicateTopics, 0, 0)
		})
	}
}
