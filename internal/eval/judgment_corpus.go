package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"github.com/kpenfound/hearsay/internal/llm"
)

// Corpus is an operator-adjudicated dataset. Text stays here, never in reports.
// Group assigns related revisions/paraphrases to one split to prevent leakage.
type Corpus struct {
	Version    int       `json:"version"`
	Provenance string    `json:"provenance"`
	Examples   []Example `json:"examples"`
}

// Example binds a human yes/no label for one judgment to immutable source
// evidence. Every task is one atomic yes/no question, so a label is yes or no.
type Example struct {
	ID       string           `json:"id"`
	Group    string           `json:"group"`
	Task     llm.JudgmentTask `json:"task"`
	Split    string           `json:"split"`
	Category string           `json:"category"`
	Sources  []CorpusSource   `json:"sources"`
	Review   Review           `json:"review"`
	Label    string           `json:"label"`
}

// CorpusSource is a revision-pinned, access-scoped source excerpt for review.
type CorpusSource struct {
	ID       string   `json:"id"`
	Revision string   `json:"revision"`
	URI      string   `json:"uri"`
	ACL      []string `json:"acl"`
	Text     string   `json:"text"`
}

// Review records who adjudicated the label, against which review revision.
type Review struct {
	Adjudicator string `json:"adjudicator"`
	Revision    string `json:"revision"`
	Rationale   string `json:"rationale"`
}

// Prediction contains supplied worker output, not a request to run a provider.
// A blank label is no answer. The baseline set is any reference predictor to
// compare Jev with, such as a generative model's implied answer: a generated
// "restates" is a yes to stance.same_answer, a generated decided outcome a yes
// to outcome.binding_agreement. It may be empty.
type Prediction struct {
	ExampleID    string   `json:"example_id"`
	ModelVersion string   `json:"model_version"`
	Label        string   `json:"label,omitempty"`
	Confidence   *float64 `json:"confidence"`
	ConflictFlag *bool    `json:"conflict_flag,omitempty"`
	ConflictReal *bool    `json:"conflict_real,omitempty"`
	Corrected    *bool    `json:"corrected,omitempty"`
}

// Predictions separates the pinned candidate from the current pipeline.
type Predictions struct {
	Jev      []Prediction `json:"jev"`
	Baseline []Prediction `json:"baseline"`
}

// DecodeEvaluation reads one strict JSON value, refusing typos and trailing data.
func DecodeEvaluation(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("decode evaluation input: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("evaluation input must contain exactly one JSON value")
	}
	return nil
}

var classes = []string{"yes", "no"}

func validExampleLabel(e Example) bool { return e.Task.Known() && slices.Contains(classes, e.Label) }

func validPrediction(p Prediction) bool { return p.Label == "" || slices.Contains(classes, p.Label) }

// Validate checks provenance and split isolation before any scoring.
func (c Corpus) Validate() error {
	if c.Version != 1 || c.Provenance == "" {
		return fmt.Errorf("corpus requires version 1 and provenance")
	}
	ids, groups := map[string]bool{}, map[string]string{}
	sources := map[string]string{}
	for i, e := range c.Examples {
		if e.ID == "" || ids[e.ID] {
			return fmt.Errorf("example %d requires a unique id", i)
		}
		ids[e.ID] = true
		if e.Group == "" || (e.Split != "tuning" && e.Split != "held_out") {
			return fmt.Errorf("example %d requires a group and tuning or held_out split", i)
		}
		if s, ok := groups[e.Group]; ok && s != e.Split {
			return fmt.Errorf("example %d leaks a group across splits", i)
		}
		groups[e.Group] = e.Split
		if !validExampleLabel(e) {
			return fmt.Errorf("example %d requires a known task and a yes or no label", i)
		}
		if e.Review.Adjudicator == "" || e.Review.Revision == "" || e.Review.Rationale == "" || len(e.Sources) == 0 {
			return fmt.Errorf("example %d requires review and source provenance", i)
		}
		for _, s := range e.Sources {
			if s.ID == "" || s.Revision == "" || s.URI == "" || s.Text == "" || len(s.ACL) == 0 {
				return fmt.Errorf("example %d requires revision-pinned source text and access scope", i)
			}
			if split, ok := sources[s.ID]; ok && split != e.Split {
				return fmt.Errorf("example %d leaks a source across splits", i)
			}
			sources[s.ID] = e.Split
		}
	}
	return nil
}

func predictionIndex(c Corpus, ps []Prediction) (map[string]Prediction, error) {
	examples := map[string]Example{}
	for _, e := range c.Examples {
		examples[e.ID] = e
	}
	out := map[string]Prediction{}
	for i, p := range ps {
		if _, ok := examples[p.ExampleID]; !ok {
			return nil, fmt.Errorf("prediction %d has unknown example id", i)
		}
		if _, ok := out[p.ExampleID]; ok {
			return nil, fmt.Errorf("prediction %d repeats an example", i)
		}
		if !validPrediction(p) {
			return nil, fmt.Errorf("prediction %d has a label other than yes, no or blank", i)
		}
		if p.Confidence != nil && (math.IsNaN(*p.Confidence) || math.IsInf(*p.Confidence, 0) || *p.Confidence < 0 || *p.Confidence > 1) {
			return nil, fmt.Errorf("prediction %d has invalid confidence", i)
		}
		out[p.ExampleID] = p
	}
	return out, nil
}

// ErrorMeasure reports an explicit denominator; an empty measure has null rate.
type ErrorMeasure struct {
	Count int      `json:"count"`
	Of    int      `json:"of"`
	Rate  *float64 `json:"rate"`
}

func measure(n, d int) ErrorMeasure {
	m := ErrorMeasure{Count: n, Of: d}
	if d > 0 {
		m.Rate = ratio(n, d)
	}
	return m
}

// JudgmentMetrics keeps each named error separate. Confusion rows are gold,
// columns predicted ("none" where no answer was given); every cell is present.
// What a false yes or a false no costs depends on the judgment: a false yes
// to topic.same_question is an incorrect attachment, a false no to
// stance.same_answer a false change.
type JudgmentMetrics struct {
	Examples          int                       `json:"examples"`
	Missing           int                       `json:"missing_predictions"`
	Unanswered        int                       `json:"unanswered"`
	Errors            ErrorMeasure              `json:"errors"`
	FalseYes          ErrorMeasure              `json:"false_yes"`
	FalseNo           ErrorMeasure              `json:"false_no"`
	Confusion         map[string]map[string]int `json:"confusion_matrix"`
	ConflictPrecision ErrorMeasure              `json:"conflict_precision"`
	Corrections       ErrorMeasure              `json:"corrections"`
}

// TaskComparison compares only supplied predictions against the same labels.
type TaskComparison struct {
	Baseline JudgmentMetrics `json:"baseline"`
	Jev      JudgmentMetrics `json:"jev"`
}

// CompareJudgments never produces predictions or includes corpus text in output.
func CompareJudgments(c Corpus, predictions Predictions, split string) (map[llm.JudgmentTask]TaskComparison, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if split != "" && split != "tuning" && split != "held_out" {
		return nil, fmt.Errorf("unknown evaluation split")
	}
	b, err := predictionIndex(c, predictions.Baseline)
	if err != nil {
		return nil, err
	}
	j, err := predictionIndex(c, predictions.Jev)
	if err != nil {
		return nil, err
	}
	out := map[llm.JudgmentTask]TaskComparison{}
	for _, task := range llm.JudgmentTasks() {
		var es []Example
		for _, e := range c.Examples {
			if e.Task == task && (split == "" || e.Split == split) {
				es = append(es, e)
			}
		}
		out[task] = TaskComparison{Baseline: scoreJudgments(es, b), Jev: scoreJudgments(es, j)}
	}
	return out, nil
}

func scoreJudgments(es []Example, ps map[string]Prediction) JudgmentMetrics {
	m := JudgmentMetrics{Confusion: map[string]map[string]int{}}
	for _, gold := range classes {
		m.Confusion[gold] = map[string]int{"yes": 0, "no": 0, "none": 0}
	}
	var errorsN, falseYes, golds, falseNo, gnos, real, verdicts, corrections, reviewed int
	for _, e := range es {
		p, ok := ps[e.ID]
		if !ok {
			m.Missing++
			continue
		}
		m.Examples++
		label := p.Label
		if label == "" {
			label = "none"
			m.Unanswered++
		}
		m.Confusion[e.Label][label]++
		if label != e.Label {
			errorsN++
		}
		switch e.Label {
		case "no":
			gnos++
			if label == "yes" {
				falseYes++
			}
		case "yes":
			golds++
			if label != "yes" {
				falseNo++
			}
		}
		if p.ConflictFlag != nil && *p.ConflictFlag && p.ConflictReal != nil {
			verdicts++
			if *p.ConflictReal {
				real++
			}
		}
		if p.Corrected != nil {
			reviewed++
			if *p.Corrected {
				corrections++
			}
		}
	}
	m.Errors = measure(errorsN, m.Examples)
	m.FalseYes = measure(falseYes, gnos)
	m.FalseNo = measure(falseNo, golds)
	m.ConflictPrecision = measure(real, verdicts)
	m.Corrections = measure(corrections, reviewed)
	return m
}
