package eval_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/llm"
)

func example(id string, task llm.JudgmentTask, label, split string) eval.Example {
	return eval.Example{ID: id, Group: id, Task: task, Split: split, Category: "hand-built", Label: label, Candidates: []string{"a", "b"}, Sources: []eval.CorpusSource{{ID: "source-" + id, Revision: "r1", URI: "fixture://" + id, ACL: []string{"public"}, Text: "source content"}}, Review: eval.Review{Adjudicator: "test-reviewer", Revision: "r1", Rationale: "hand-built label"}}
}
func corpus(es ...eval.Example) eval.Corpus {
	return eval.Corpus{Version: 1, Provenance: "hand-built test", Examples: es}
}
func prediction(id, label string) eval.Prediction {
	return eval.Prediction{ExampleID: id, ModelVersion: "test-v1", Label: label, Confidence: share(.9)}
}
func readFixture(t *testing.T, path string, v any) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := eval.DecodeEvaluation(f, v); err != nil {
		t.Fatal(err)
	}
}

func TestNamedJudgmentErrors(t *testing.T) {
	tests := []struct {
		name       string
		task       llm.JudgmentTask
		gold, pred []string
		check      func(*testing.T, eval.JudgmentMetrics)
	}{
		{"stance", llm.TaskStanceComparison, []string{"restates", "restates", "changes", "changes"}, []string{"changes", "restates", "restates", "changes"}, func(t *testing.T, m eval.JudgmentMetrics) {
			assertMeasure(t, m.FalseChanges, 1, 2)
			assertMeasure(t, m.MissedChanges, 1, 2)
			assertMeasure(t, m.Errors, 2, 4)
		}},
		{"attachment and duplicates", llm.TaskTopicMatching, []string{"a", "a", "none_fit", "a"}, []string{"b", "new", "a", "a"}, func(t *testing.T, m eval.JudgmentMetrics) {
			assertMeasure(t, m.IncorrectAttachments, 2, 3)
			assertMeasure(t, m.DuplicateTopics, 1, 3)
		}},
		{"new topic when none fits", llm.TaskTopicMatching, []string{"none_fit"}, []string{"new"}, func(t *testing.T, m eval.JudgmentMetrics) {
			assertMeasure(t, m.Errors, 0, 1)
			assertMeasure(t, m.DuplicateTopics, 0, 0)
			assertMeasure(t, m.IncorrectAttachments, 0, 0)
		}},
		{"shortlist", llm.TaskTopicShortlist, []string{"a", "none_fit"}, []string{"b", "none_fit"}, func(t *testing.T, m eval.JudgmentMetrics) {
			assertMeasure(t, m.IncorrectAttachments, 1, 1)
			assertMeasure(t, m.DuplicateTopics, 0, 1)
		}},
		{"outcome", llm.TaskOutcomeClassification, []string{"resolved", "decided", "proposed", "open", "none"}, []string{"open", "resolved", "decided", "open", "none"}, func(t *testing.T, m eval.JudgmentMetrics) {
			assertMeasure(t, m.OutcomeConfusion, 3, 5)
			assertMeasure(t, m.MissedDecisions, 1, 2)
			if len(m.Confusion) != 5 || m.Confusion["resolved"]["open"] != 1 || m.Confusion["decided"]["resolved"] != 1 || m.Confusion["proposed"]["decided"] != 1 {
				t.Fatalf("confusion=%v", m.Confusion)
			}
			for _, row := range m.Confusion {
				if len(row) != 6 {
					t.Fatal("incomplete confusion matrix")
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var es []eval.Example
			var ps []eval.Prediction
			for i, gold := range tt.gold {
				id := string(rune('A' + i))
				es = append(es, example(id, tt.task, gold, "held_out"))
				ps = append(ps, prediction(id, tt.pred[i]))
			}
			got, err := eval.CompareJudgments(corpus(es...), eval.Predictions{Baseline: ps, Jev: ps}, "held_out")
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, got[tt.task].Jev)
			if !reflect.DeepEqual(got[tt.task].Baseline, got[tt.task].Jev) {
				t.Fatal("baseline and Jev scoring differ")
			}
		})
	}
}

func assertMeasure(t *testing.T, m eval.ErrorMeasure, n, d int) {
	t.Helper()
	if m.Count != n || m.Of != d {
		t.Fatalf("measure=%+v, want %d/%d", m, n, d)
	}
	if d == 0 {
		if m.Rate != nil {
			t.Fatalf("empty rate=%v", m.Rate)
		}
	} else if m.Rate == nil || *m.Rate != float64(n)/float64(d) {
		t.Fatalf("rate=%v", m.Rate)
	}
}

func TestDownstreamAndEmptyJudgments(t *testing.T) {
	c := corpus(example("a", llm.TaskStanceComparison, "changes", "held_out"), example("b", llm.TaskStanceComparison, "restates", "held_out"), example("missing", llm.TaskStanceComparison, "restates", "held_out"))
	yes, no := true, false
	a, b := prediction("a", "changes"), prediction("b", "restates")
	a.ConflictFlag = &yes
	a.ConflictReal = &yes
	a.Corrected = &no
	b.ConflictFlag = &yes
	b.ConflictReal = &no
	b.Corrected = &yes
	got, err := eval.CompareJudgments(c, eval.Predictions{Jev: []eval.Prediction{a, b}}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := got[llm.TaskStanceComparison].Jev
	assertMeasure(t, m.ConflictPrecision, 1, 2)
	assertMeasure(t, m.Corrections, 1, 2)
	if m.Missing != 1 {
		t.Fatal(m)
	}
	for task, c := range got {
		m := c.Baseline
		for _, v := range []eval.ErrorMeasure{m.Errors, m.FalseChanges, m.MissedChanges, m.IncorrectAttachments, m.DuplicateTopics, m.OutcomeConfusion, m.MissedDecisions, m.ConflictPrecision, m.Corrections} {
			assertMeasure(t, v, 0, 0)
		}
		if task == llm.TaskStanceComparison && m.Missing != 3 {
			t.Fatal(m)
		}
	}
}

func TestSeedRecordedFixtures(t *testing.T) {
	var c eval.Corpus
	var ps eval.Predictions
	var responses []llm.JudgmentResponse
	readFixture(t, "testdata/seed-corpus.json", &c)
	readFixture(t, "testdata/seed-predictions.json", &ps)
	readFixture(t, "testdata/seed-judgments.json", &responses)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	categories := map[string]bool{}
	steps := []llm.JudgmentStep{}
	for _, r := range responses {
		steps = append(steps, llm.JudgmentStep{Response: r})
	}
	fake := llm.NewFakeJudgmentProvider(steps...)
	judge, err := llm.NewJudgments(llm.JudgmentConfig{Enabled: true, Model: "fixture-judge-v1"}, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fixture-only" })
	if err != nil {
		t.Fatal(err)
	}
	var replayed []eval.Prediction
	for _, e := range c.Examples {
		categories[e.Category] = true
		opts := []string{"changes", "restates", "abstain"}
		switch e.Task {
		case llm.TaskOutcomeClassification:
			opts = []string{"resolved", "decided", "proposed", "open", "none"}
		case llm.TaskTopicMatching, llm.TaskTopicShortlist:
			opts = append(slices.Clone(e.Candidates), "none_fit")
		}
		r := judge.Choice(t.Context(), e.Sources[0].Text, llm.JudgmentQuestion{ID: e.ID, Question: "Classify the recorded source under the task rubric", Options: opts})
		if r.Fallback != "" || len(r.Answers) != 1 {
			t.Fatalf("%s: %+v", e.ID, r)
		}
		a := r.Answers[0]
		replayed = append(replayed, eval.Prediction{ExampleID: e.ID, ModelVersion: r.ModelVersion, Label: a.Choice, Confidence: &a.Confidence})
	}
	for _, category := range []string{"paraphrase", "negation", "proposal_versus_decision", "source_edits", "private_evidence", "adversarial_source_text"} {
		if !categories[category] {
			t.Fatalf("missing category %s", category)
		}
	}
	if fake.Calls() != len(c.Examples) {
		t.Fatal("fixtures were not all replayed")
	}
	ps.Jev = replayed
	report, err := eval.CompareJudgments(c, ps, "held_out")
	if err != nil {
		t.Fatal(err)
	}
	for task, m := range report {
		if m.Jev.Examples == 0 || m.Jev.Errors.Count != 0 || m.Baseline.Errors.Count == 0 {
			t.Fatalf("%s: %+v", task, m)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range c.Examples {
		for _, s := range e.Sources {
			if bytes.Contains(encoded, []byte(s.Text)) {
				t.Fatal("report leaked source text")
			}
		}
	}
}

func TestCorpusRefusals(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*eval.Corpus)
		want   string
	}{
		{"label", func(c *eval.Corpus) { c.Examples[0].Label = "" }, "adjudicated"},
		{"review", func(c *eval.Corpus) { c.Examples[0].Review.Adjudicator = "" }, "review"},
		{"source revision", func(c *eval.Corpus) { c.Examples[0].Sources[0].Revision = "" }, "revision"},
		{"group leak", func(c *eval.Corpus) {
			e := example("b", llm.TaskStanceComparison, "restates", "held_out")
			e.Group = c.Examples[0].Group
			c.Examples = append(c.Examples, e)
		}, "leaks"},
		{"source leak", func(c *eval.Corpus) {
			e := example("b", llm.TaskStanceComparison, "restates", "held_out")
			e.Sources[0].ID = c.Examples[0].Sources[0].ID
			c.Examples = append(c.Examples, e)
		}, "leaks"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := corpus(example("a", llm.TaskStanceComparison, "restates", "tuning"))
			tt.mutate(&c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate=%v", err)
			}
		})
	}
	for _, s := range []string{`{"version":1,"typo":1}`, `{} {}`} {
		var c eval.Corpus
		if err := eval.DecodeEvaluation(strings.NewReader(s), &c); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}
