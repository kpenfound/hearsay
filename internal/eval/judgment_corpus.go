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

// Example binds a human label to immutable source evidence and a task.
type Example struct {
	ID         string           `json:"id"`
	Group      string           `json:"group"`
	Task       llm.JudgmentTask `json:"task"`
	Split      string           `json:"split"`
	Category   string           `json:"category"`
	Sources    []CorpusSource   `json:"sources"`
	Review     Review           `json:"review"`
	Label      string           `json:"label"`
	Candidates []string         `json:"candidates,omitempty"`
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
// A blank label or abstain is an abstention; qualification uses the baseline.
type Prediction struct {
	ExampleID    string   `json:"example_id"`
	ModelVersion string   `json:"model_version"`
	Label        string   `json:"label"`
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

func taskClasses(task llm.JudgmentTask) []string {
	switch task {
	case llm.TaskStanceComparison:
		return []string{"changes", "restates"}
	case llm.TaskTopicMatching, llm.TaskTopicShortlist:
		return []string{"existing", "none_fit"}
	case llm.TaskOutcomeClassification:
		return []string{"resolved", "decided", "proposed", "open", "none"}
	}
	return nil
}

func exampleClass(e Example) string {
	if (e.Task == llm.TaskTopicMatching || e.Task == llm.TaskTopicShortlist) && e.Label != "none_fit" {
		return "existing"
	}
	return e.Label
}

func validLabel(e Example, label string, predicted bool) bool {
	if predicted && (label == "" || label == "abstain") {
		return true
	}
	switch e.Task {
	case llm.TaskTopicMatching, llm.TaskTopicShortlist:
		return label == "none_fit" || predicted && label == "new" || slices.Contains(e.Candidates, label)
	default:
		return slices.Contains(taskClasses(e.Task), label)
	}
}

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
		if len(taskClasses(e.Task)) == 0 || !validLabel(e, e.Label, false) {
			return fmt.Errorf("example %d requires an adjudicated task label", i)
		}
		if e.Review.Adjudicator == "" || e.Review.Revision == "" || e.Review.Rationale == "" || len(e.Sources) == 0 {
			return fmt.Errorf("example %d requires review and source provenance", i)
		}
		candidates := map[string]bool{}
		for _, id := range e.Candidates {
			if id == "" || id == "none_fit" || id == "new" || id == "abstain" || candidates[id] {
				return fmt.Errorf("example %d has invalid candidate ids", i)
			}
			candidates[id] = true
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
		e, ok := examples[p.ExampleID]
		if !ok {
			return nil, fmt.Errorf("prediction %d has unknown example id", i)
		}
		if _, ok := out[p.ExampleID]; ok {
			return nil, fmt.Errorf("prediction %d repeats an example", i)
		}
		if !validLabel(e, p.Label, true) {
			return nil, fmt.Errorf("prediction %d has invalid task label", i)
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
// columns predicted; every outcome cell is present, including zero cells.
type JudgmentMetrics struct {
	Examples             int                       `json:"examples"`
	Missing              int                       `json:"missing_predictions"`
	Errors               ErrorMeasure              `json:"errors"`
	FalseChanges         ErrorMeasure              `json:"false_changes"`
	MissedChanges        ErrorMeasure              `json:"missed_changes"`
	IncorrectAttachments ErrorMeasure              `json:"incorrect_attachments"`
	DuplicateTopics      ErrorMeasure              `json:"duplicate_topics"`
	OutcomeConfusion     ErrorMeasure              `json:"outcome_confusion"`
	Confusion            map[string]map[string]int `json:"confusion_matrix"`
	MissedDecisions      ErrorMeasure              `json:"missed_decisions"`
	ConflictPrecision    ErrorMeasure              `json:"conflict_precision"`
	Corrections          ErrorMeasure              `json:"corrections"`
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
	for _, task := range []llm.JudgmentTask{llm.TaskStanceComparison, llm.TaskTopicMatching, llm.TaskTopicShortlist, llm.TaskOutcomeClassification} {
		var es []Example
		for _, e := range c.Examples {
			if e.Task == task && (split == "" || e.Split == split) {
				es = append(es, e)
			}
		}
		out[task] = TaskComparison{Baseline: scoreJudgments(task, es, b), Jev: scoreJudgments(task, es, j)}
	}
	return out, nil
}

func scoreJudgments(task llm.JudgmentTask, es []Example, ps map[string]Prediction) JudgmentMetrics {
	m := JudgmentMetrics{Confusion: map[string]map[string]int{}}
	if task == llm.TaskOutcomeClassification {
		for _, gold := range taskClasses(task) {
			m.Confusion[gold] = map[string]int{}
			for _, label := range append(taskClasses(task), "abstain") {
				m.Confusion[gold][label] = 0
			}
		}
	}
	var errorsN, fc, restates, mc, changes, ia, attachments, dup, existing, oc, outcomes, md, decisions, real, verdicts, corrections, reviewed int
	for _, e := range es {
		p, ok := ps[e.ID]
		if !ok {
			m.Missing++
			continue
		}
		m.Examples++
		label := p.Label
		if label == "" {
			label = "abstain"
		}
		// A generative new-topic output and none-fit both mean no existing
		// attachment. Creation is a duplicate only when an existing candidate fits.
		correct := label == e.Label || (task == llm.TaskTopicMatching || task == llm.TaskTopicShortlist) && label == "new" && e.Label == "none_fit"
		if !correct {
			errorsN++
		}
		switch task {
		case llm.TaskStanceComparison:
			if e.Label == "restates" {
				restates++
				if label == "changes" {
					fc++
				}
			}
			if e.Label == "changes" {
				changes++
				if label != "changes" {
					mc++
				}
			}
		case llm.TaskTopicMatching, llm.TaskTopicShortlist:
			if label != "none_fit" && label != "new" && label != "abstain" {
				attachments++
				if label != e.Label {
					ia++
				}
			}
			if e.Label != "none_fit" {
				existing++
				if label == "new" {
					dup++
				}
			}
		case llm.TaskOutcomeClassification:
			outcomes++
			m.Confusion[e.Label][label]++
			if label != e.Label {
				oc++
			}
			if e.Label == "decided" || e.Label == "resolved" {
				decisions++
				if label != "decided" && label != "resolved" {
					md++
				}
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
	m.FalseChanges = measure(fc, restates)
	m.MissedChanges = measure(mc, changes)
	m.IncorrectAttachments = measure(ia, attachments)
	m.DuplicateTopics = measure(dup, existing)
	m.OutcomeConfusion = measure(oc, outcomes)
	m.MissedDecisions = measure(md, decisions)
	m.ConflictPrecision = measure(real, verdicts)
	m.Corrections = measure(corrections, reviewed)
	return m
}
