package eval_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/llm"
)

func qualificationInput() (eval.Corpus, eval.Predictions, eval.QualificationOptions) {
	es := []eval.Example{example("t-change", llm.TaskStanceComparison, "changes", "tuning"), example("t-restate", llm.TaskStanceComparison, "restates", "tuning"), example("h-change", llm.TaskStanceComparison, "changes", "held_out"), example("h-restate", llm.TaskStanceComparison, "restates", "held_out")}
	ps := eval.Predictions{}
	for _, e := range es {
		ps.Jev = append(ps.Jev, prediction(e.ID, e.Label))
		wrong := "changes"
		if e.Label == "changes" {
			wrong = "restates"
		}
		ps.Baseline = append(ps.Baseline, prediction(e.ID, wrong))
	}
	return corpus(es...), ps, eval.QualificationOptions{Task: llm.TaskStanceComparison, Name: "test-only", ModelVersion: "test-v1", Limits: map[string]float64{"false_changes": 0, "missed_changes": 0}, MinimumPerClass: map[string]int{"changes": 1, "restates": 1}}
}

func TestQualificationOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name, status string
		mutate       func(*eval.Corpus, *eval.Predictions, *eval.QualificationOptions)
		failed       []string
	}{
		{"passes", "qualified", func(*eval.Corpus, *eval.Predictions, *eval.QualificationOptions) {}, nil},
		{"held out fails limits", "failed_qualification", func(_ *eval.Corpus, p *eval.Predictions, _ *eval.QualificationOptions) {
			p.Jev[2].Label = "restates"
			p.Jev[3].Label = "changes"
		}, []string{"false_changes", "missed_changes"}},
		{"too few held out", "insufficient_evidence", func(_ *eval.Corpus, _ *eval.Predictions, o *eval.QualificationOptions) {
			o.MinimumPerClass["changes"] = 2
		}, nil},
		{"too few adopted", "insufficient_evidence", func(_ *eval.Corpus, p *eval.Predictions, _ *eval.QualificationOptions) {
			p.Jev[2].Confidence = share(.1)
		}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, p, o := qualificationInput()
			tt.mutate(&c, &p, &o)
			q, err := eval.Qualify(c, p, o)
			if err != nil {
				t.Fatal(err)
			}
			if q.Status != tt.status || len(q.Policies) > 0 != (tt.status == "qualified") {
				t.Fatalf("qualification=%+v", q)
			}
			if tt.failed != nil && !reflect.DeepEqual(q.FailedLimits, tt.failed) {
				t.Fatalf("failed=%v", q.FailedLimits)
			}
			if tt.status == "insufficient_evidence" && len(q.InsufficientClasses) == 0 {
				t.Fatal("missing evidence reasons")
			}
			if tt.status == "qualified" {
				assertConfigPolicy(t, q)
			}
		})
	}
}

func assertConfigPolicy(t *testing.T, q eval.Qualification) {
	t.Helper()
	root := t.TempDir()
	data, err := yaml.Marshal(map[string]any{"sources": []map[string]any{{"id": "github", "type": "github", "containers": []string{"acme/api"}}}, "scopes": []map[string]any{{"id": "api", "sources": []string{"github"}}}, "llm": map[string]any{"boost": map[string]any{"enabled": true, "model": q.ModelVersion, "policies": q.Policies}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "hearsay.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	repo, err := config.Load(path)
	if err != nil {
		t.Fatalf("emitted policy rejected by config: %v\n%s", err, data)
	}
	p := repo.Boost.Policies[q.Task]
	if !p.Accepts(q.ModelVersion, q.Threshold) || p.Accepts("other-version", 1) {
		t.Fatalf("policy=%+v", p)
	}
}

func TestQualificationRefusals(t *testing.T) {
	for _, tt := range []struct {
		name, want string
		mutate     func(*eval.Corpus, *eval.Predictions, *eval.QualificationOptions)
	}{
		{"no held out", "held-out labels", func(c *eval.Corpus, p *eval.Predictions, _ *eval.QualificationOptions) {
			c.Examples = c.Examples[:2]
			p.Jev = p.Jev[:2]
			p.Baseline = p.Baseline[:2]
		}},
		{"unlabeled held out", "adjudicated", func(c *eval.Corpus, _ *eval.Predictions, _ *eval.QualificationOptions) { c.Examples[2].Label = "" }},
		{"no limits", "operator limits", func(_ *eval.Corpus, _ *eval.Predictions, o *eval.QualificationOptions) { o.Limits = nil }},
		{"wrong model", "model version mismatch", func(_ *eval.Corpus, p *eval.Predictions, _ *eval.QualificationOptions) {
			p.Jev[2].ModelVersion = "test-v2"
		}},
		{"no evidence limits", "minimum_per_class", func(_ *eval.Corpus, _ *eval.Predictions, o *eval.QualificationOptions) { o.MinimumPerClass = nil }},
		{"wrong task limit", "invalid operator limit", func(_ *eval.Corpus, _ *eval.Predictions, o *eval.QualificationOptions) {
			o.Limits = map[string]float64{"outcome_confusion": 0}
		}},
		{"no candidate prediction", "complete", func(_ *eval.Corpus, p *eval.Predictions, _ *eval.QualificationOptions) { p.Jev = p.Jev[:3] }},
		{"unknown confidence", "confidence", func(_ *eval.Corpus, p *eval.Predictions, _ *eval.QualificationOptions) { p.Jev[2].Confidence = nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, p, o := qualificationInput()
			tt.mutate(&c, &p, &o)
			q, err := eval.Qualify(c, p, o)
			if err == nil || !strings.Contains(err.Error(), tt.want) || len(q.Policies) > 0 {
				t.Fatalf("q=%+v err=%v", q, err)
			}
		})
	}
}

func TestHeldOutLabelsCannotChangeThreshold(t *testing.T) {
	c, p, o := qualificationInput()
	// Tuning has one low-confidence wrong answer, which must fall back.
	p.Jev[0].Confidence = share(.2)
	p.Jev[0].Label = "restates"
	p.Baseline[0].Label = "changes"
	before, err := eval.Qualify(c, p, o)
	if err != nil {
		t.Fatal(err)
	}
	c.Examples[2].Label = "restates"
	c.Examples[3].Label = "changes"
	after, err := eval.Qualify(c, p, o)
	if err != nil {
		t.Fatal(err)
	}
	if before.Threshold != after.Threshold || !reflect.DeepEqual(before.Tuning, after.Tuning) || before.Status != "qualified" || after.Status != "failed_qualification" {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
	if before.Threshold <= .2 || before.Threshold > .9 {
		t.Fatalf("threshold=%v", before.Threshold)
	}
}

func TestQualificationAllTasksAndFallback(t *testing.T) {
	var seed eval.Corpus
	var ps eval.Predictions
	readFixture(t, "testdata/seed-corpus.json", &seed)
	readFixture(t, "testdata/seed-predictions.json", &ps)
	for _, tt := range []struct {
		task    llm.JudgmentTask
		classes []string
		limits  map[string]float64
	}{
		{llm.TaskOutcomeClassification, []string{"resolved", "decided", "proposed", "open", "none"}, map[string]float64{"outcome_confusion": 0, "missed_decisions": 0}},
		// none_fit falls back to baseline. Use its correct baseline for this case.
		{llm.TaskTopicMatching, []string{"existing", "none_fit"}, map[string]float64{"incorrect_attachments": 0, "duplicate_topics": 0}},
	} {
		t.Run(string(tt.task), func(t *testing.T) {
			p := ps
			p.Baseline = slices.Clone(ps.Baseline)
			for i, e := range seed.Examples {
				if e.Label == "none_fit" {
					p.Baseline[i].Label = "none_fit"
				}
			}
			minimum := map[string]int{}
			for _, class := range tt.classes {
				minimum[class] = 1
			}
			q, err := eval.Qualify(seed, p, eval.QualificationOptions{Task: tt.task, Name: "test-only", ModelVersion: "fixture-judge-v1", Limits: tt.limits, MinimumPerClass: minimum})
			if err != nil || q.Status != "qualified" {
				t.Fatalf("q=%+v err=%v", q, err)
			}
			assertConfigPolicy(t, q)
		})
	}
}

func TestOperatorLimitsRequireExplicitNumbers(t *testing.T) {
	for _, tt := range []struct{ name, data, want string }{
		{"null limit", `{"task":"stance_comparison","limits":{"false_changes":null}}`, "explicit number"},
		{"typo", `{"task":"stance_comparison","limits":{"false_changes":0},"typo":true}`, "unknown field"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var options eval.QualificationOptions
			err := eval.DecodeEvaluation(strings.NewReader(tt.data), &options)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("options=%+v err=%v", options, err)
			}
		})
	}
}
