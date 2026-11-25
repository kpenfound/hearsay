package eval_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/llm"
)

func example(id string, task llm.JudgmentTask, label, split string) eval.Example {
	return eval.Example{ID: id, Group: id, Task: task, Split: split, Category: "hand-built", Label: label, Sources: []eval.CorpusSource{{ID: "source-" + id, Revision: "r1", URI: "fixture://" + id, ACL: []string{"public"}, Text: "source content"}}, Review: eval.Review{Adjudicator: "test-reviewer", Revision: "r1", Rationale: "hand-built label"}}
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
		gold, pred []string
		check      func(*testing.T, eval.JudgmentMetrics)
	}{
		{"false yes and false no", []string{"no", "no", "yes", "yes"}, []string{"yes", "no", "no", "yes"}, func(t *testing.T, m eval.JudgmentMetrics) {
			assertMeasure(t, m.FalseYes, 1, 2)
			assertMeasure(t, m.FalseNo, 1, 2)
			assertMeasure(t, m.Errors, 2, 4)
		}},
		{"unanswered is an error and decides nothing", []string{"yes", "no"}, []string{"", ""}, func(t *testing.T, m eval.JudgmentMetrics) {
			assertMeasure(t, m.Errors, 2, 2)
			assertMeasure(t, m.FalseNo, 1, 1)
			assertMeasure(t, m.FalseYes, 0, 1)
			if m.Unanswered != 2 {
				t.Fatalf("unanswered=%d", m.Unanswered)
			}
		}},
		{"confusion matrix is complete", []string{"yes", "no", "no"}, []string{"yes", "yes", ""}, func(t *testing.T, m eval.JudgmentMetrics) {
			if m.Confusion["yes"]["yes"] != 1 || m.Confusion["no"]["yes"] != 1 || m.Confusion["no"]["none"] != 1 {
				t.Fatalf("confusion=%v", m.Confusion)
			}
			for _, row := range m.Confusion {
				if len(row) != 3 {
					t.Fatal("incomplete confusion matrix")
				}
			}
		}},
	}
	for _, task := range llm.JudgmentTasks() {
		for _, tt := range tests {
			t.Run(string(task)+"/"+tt.name, func(t *testing.T) {
				var es []eval.Example
				var ps []eval.Prediction
				for i, gold := range tt.gold {
					id := string(rune('A' + i))
					es = append(es, example(id, task, gold, "held_out"))
					ps = append(ps, prediction(id, tt.pred[i]))
				}
				got, err := eval.CompareJudgments(corpus(es...), eval.Predictions{Baseline: ps, Jev: ps}, "held_out")
				if err != nil {
					t.Fatal(err)
				}
				tt.check(t, got[task].Jev)
				if !reflect.DeepEqual(got[task].Baseline, got[task].Jev) {
					t.Fatal("baseline and Jev scoring differ")
				}
			})
		}
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

func TestDownstreamJudgmentMeasures(t *testing.T) {
	c := corpus(example("a", llm.TaskStanceSameAnswer, "no", "held_out"), example("b", llm.TaskStanceSameAnswer, "yes", "held_out"))
	yes, no := true, false
	a, b := prediction("a", "no"), prediction("b", "yes")
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
	m := got[llm.TaskStanceSameAnswer].Jev
	assertMeasure(t, m.ConflictPrecision, 1, 2)
	assertMeasure(t, m.Corrections, 1, 2)
}

func TestExamplesWithoutPredictionsAreMissingAndMeasureNothing(t *testing.T) {
	c := corpus(example("a", llm.TaskStanceSameAnswer, "no", "held_out"), example("b", llm.TaskStanceSameAnswer, "yes", "held_out"), example("missing", llm.TaskStanceSameAnswer, "yes", "held_out"))
	got, err := eval.CompareJudgments(c, eval.Predictions{Jev: []eval.Prediction{prediction("a", "no"), prediction("b", "yes")}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if m := got[llm.TaskStanceSameAnswer].Jev; m.Missing != 1 {
		t.Fatalf("jev missing = %d, want 1", m.Missing)
	}
	for task, c := range got {
		m := c.Baseline
		for _, v := range []eval.ErrorMeasure{m.Errors, m.FalseYes, m.FalseNo, m.ConflictPrecision, m.Corrections} {
			assertMeasure(t, v, 0, 0)
		}
		if task == llm.TaskStanceSameAnswer && m.Missing != 3 {
			t.Fatal(m)
		}
	}
}

type seedFixtures struct {
	corpus    eval.Corpus
	recorded  eval.Predictions
	responses []llm.JudgmentResponse
}

func readSeedFixtures(t *testing.T) seedFixtures {
	t.Helper()
	var f seedFixtures
	readFixture(t, "testdata/seed-corpus.json", &f.corpus)
	readFixture(t, "testdata/seed-predictions.json", &f.recorded)
	readFixture(t, "testdata/seed-judgments.json", &f.responses)
	return f
}

func (f seedFixtures) replay(t *testing.T) (predictions []eval.Prediction, calls int) {
	t.Helper()
	steps := []llm.JudgmentStep{}
	for _, r := range f.responses {
		steps = append(steps, llm.JudgmentStep{Response: r})
	}
	fake := llm.NewFakeJudgmentProvider(steps...)
	judge, err := llm.NewJudgments(llm.JudgmentConfig{Model: "fixture-judge-v1"}, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fixture-only" })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.corpus.Examples {
		r := judge.YesNo(t.Context(), e.Sources[0].Text, llm.JudgmentQuestion{ID: e.ID, Question: "Answer the recorded source's judgment"})
		if r.Failure != "" || len(r.Answers) != 1 || r.Answers[0].Yes == nil {
			t.Fatalf("%s: %+v", e.ID, r)
		}
		label := "no"
		if *r.Answers[0].Yes {
			label = "yes"
		}
		predictions = append(predictions, eval.Prediction{ExampleID: e.ID, ModelVersion: r.ModelVersion, Label: label, Confidence: share(r.Answers[0].Confidence)})
	}
	return predictions, fake.Calls()
}

func TestSeedCorpusIsValidAndCoversEveryCategory(t *testing.T) {
	f := readSeedFixtures(t)
	if err := f.corpus.Validate(); err != nil {
		t.Fatal(err)
	}
	categories := map[string]bool{}
	for _, e := range f.corpus.Examples {
		categories[e.Category] = true
	}
	for _, category := range []string{"paraphrase", "negation", "proposal_versus_decision", "source_edits", "private_evidence", "adversarial_source_text"} {
		if !categories[category] {
			t.Errorf("missing category %s", category)
		}
	}
}

func TestSeedJudgmentFixturesReplayToTheRecordedPredictions(t *testing.T) {
	f := readSeedFixtures(t)
	replayed, calls := f.replay(t)
	if calls != len(f.responses) {
		t.Errorf("replayed %d of %d recorded responses", calls, len(f.responses))
	}
	if !reflect.DeepEqual(f.recorded.Jev, replayed) {
		t.Error("replayed predictions differ from recorded selections")
	}
}

func TestSeedReportScoresJevBetterThanTheBaseline(t *testing.T) {
	f := readSeedFixtures(t)
	f.recorded.Jev, _ = f.replay(t)
	report, err := eval.CompareJudgments(f.corpus, f.recorded, "held_out")
	if err != nil {
		t.Fatal(err)
	}
	for task, m := range report {
		if m.Jev.Examples == 0 || m.Jev.Errors.Count != 0 || m.Baseline.Errors.Count == 0 {
			t.Errorf("%s: jev=%+v baseline errors=%+v", task, m.Jev, m.Baseline.Errors)
		}
	}
}

func TestSeedReportLeaksNoSourceText(t *testing.T) {
	f := readSeedFixtures(t)
	report, err := eval.CompareJudgments(f.corpus, f.recorded, "held_out")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.corpus.Examples {
		for _, s := range e.Sources {
			if bytes.Contains(encoded, []byte(s.Text)) {
				t.Fatalf("report leaked source text of %s", e.ID)
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
		{"label", func(c *eval.Corpus) { c.Examples[0].Label = "" }, "yes or no label"},
		{"multi-way label", func(c *eval.Corpus) { c.Examples[0].Label = "restates" }, "yes or no label"},
		{"unknown task", func(c *eval.Corpus) { c.Examples[0].Task = "stance_comparison" }, "known task"},
		{"review", func(c *eval.Corpus) { c.Examples[0].Review.Adjudicator = "" }, "review"},
		{"source revision", func(c *eval.Corpus) { c.Examples[0].Sources[0].Revision = "" }, "revision"},
		{"group leak", func(c *eval.Corpus) {
			e := example("b", llm.TaskStanceSameAnswer, "yes", "held_out")
			e.Group = c.Examples[0].Group
			c.Examples = append(c.Examples, e)
		}, "leaks"},
		{"source leak", func(c *eval.Corpus) {
			e := example("b", llm.TaskStanceSameAnswer, "yes", "held_out")
			e.Sources[0].ID = c.Examples[0].Sources[0].ID
			c.Examples = append(c.Examples, e)
		}, "leaks"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := corpus(example("a", llm.TaskStanceSameAnswer, "yes", "tuning"))
			tt.mutate(&c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate=%v", err)
			}
		})
	}
}

func TestDecodeEvaluationRefusesUnknownFieldsAndTrailingValues(t *testing.T) {
	for _, s := range []string{`{"version":1,"typo":1}`, `{} {}`} {
		t.Run(s, func(t *testing.T) {
			var c eval.Corpus
			if err := eval.DecodeEvaluation(strings.NewReader(s), &c); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}
