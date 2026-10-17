package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/llm"
)

// QualificationOptions contains operator choices; there are no default limits.
// MinimumPerClass counts adopted held-out judgments by gold class. For topic
// tasks classes are existing and none_fit. Only topic_matching none_fit falls back.
type QualificationOptions struct {
	Task            llm.JudgmentTask   `json:"task"`
	ModelVersion    string             `json:"model_version"`
	Name            string             `json:"name"`
	Limits          map[string]float64 `json:"limits"`
	MinimumPerClass map[string]int     `json:"minimum_per_class"`
}

// UnmarshalJSON requires numeric limits explicitly: encoding/json normally
// turns a JSON null decoded into float64 into zero, inventing an operator limit.
func (o *QualificationOptions) UnmarshalJSON(data []byte) error {
	var wire struct {
		Task            llm.JudgmentTask    `json:"task"`
		ModelVersion    string              `json:"model_version"`
		Name            string              `json:"name"`
		Limits          map[string]*float64 `json:"limits"`
		MinimumPerClass map[string]int      `json:"minimum_per_class"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return fmt.Errorf("decode operator options: %w", err)
	}
	limits := map[string]float64{}
	for name, value := range wire.Limits {
		if value == nil {
			return fmt.Errorf("operator limit %q must be an explicit number", name)
		}
		limits[name] = *value
	}
	*o = QualificationOptions{Task: wire.Task, ModelVersion: wire.ModelVersion, Name: wire.Name, Limits: limits, MinimumPerClass: wire.MinimumPerClass}
	return nil
}

// QualifiedPolicy is directly pasteable below llm.boost.policies in config.
type QualifiedPolicy struct {
	Name         string  `json:"name" yaml:"name"`
	ModelVersion string  `json:"model_version" yaml:"model_version"`
	Threshold    float64 `json:"threshold" yaml:"threshold"`
}

// Qualification distinguishes an inadequate sample from measured failure.
// Policies is absent unless every operator limit and evidence requirement passes.
type Qualification struct {
	Status              string                               `json:"status"`
	Task                llm.JudgmentTask                     `json:"task"`
	ModelVersion        string                               `json:"model_version"`
	Threshold           float64                              `json:"threshold"`
	Tuning              JudgmentMetrics                      `json:"tuning"`
	HeldOut             JudgmentMetrics                      `json:"held_out"`
	HeldOutPerClass     map[string]int                       `json:"held_out_per_class"`
	AdoptedPerClass     map[string]int                       `json:"adopted_per_class"`
	Limits              map[string]float64                   `json:"limits"`
	MinimumPerClass     map[string]int                       `json:"minimum_per_class"`
	FailedLimits        []string                             `json:"failed_limits"`
	InsufficientClasses []string                             `json:"insufficient_classes"`
	Policies            map[llm.JudgmentTask]QualifiedPolicy `json:"policies,omitempty"`
}

func metricRates(m JudgmentMetrics) map[string]*float64 {
	return map[string]*float64{"shortlist_omissions": m.ShortlistOmissions.Rate, "errors": m.Errors.Rate, "false_changes": m.FalseChanges.Rate, "missed_changes": m.MissedChanges.Rate, "incorrect_attachments": m.IncorrectAttachments.Rate, "duplicate_topics": m.DuplicateTopics.Rate, "outcome_confusion": m.OutcomeConfusion.Rate, "missed_decisions": m.MissedDecisions.Rate, "corrections": m.Corrections.Rate}
}

func allowedLimits(task llm.JudgmentTask) []string {
	switch task {
	case llm.TaskStanceComparison:
		return []string{"errors", "false_changes", "missed_changes", "corrections"}
	case llm.TaskTopicShortlist:
		return []string{"errors", "incorrect_attachments", "shortlist_omissions", "corrections"}
	case llm.TaskTopicMatching:
		return []string{"errors", "incorrect_attachments", "duplicate_topics", "corrections"}
	case llm.TaskOutcomeClassification:
		return []string{"errors", "outcome_confusion", "missed_decisions", "corrections"}
	}
	return nil
}

func validateQualification(o QualificationOptions) error {
	if len(taskClasses(o.Task)) == 0 {
		return fmt.Errorf("qualification requires a known task")
	}
	if strings.TrimSpace(o.Name) == "" || strings.TrimSpace(o.ModelVersion) == "" || o.ModelVersion == "latest" || o.ModelVersion == "auto" {
		return fmt.Errorf("qualification requires a name and pinned model version")
	}
	if len(o.Limits) == 0 {
		return fmt.Errorf("qualification requires operator limits")
	}
	for key, limit := range o.Limits {
		if !slices.Contains(allowedLimits(o.Task), key) || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 || limit > 1 {
			return fmt.Errorf("invalid operator limit %q for task", key)
		}
	}
	for _, class := range taskClasses(o.Task) {
		if o.MinimumPerClass[class] < 1 {
			return fmt.Errorf("operator minimum_per_class must be positive for %q", class)
		}
	}
	for class := range o.MinimumPerClass {
		if !slices.Contains(taskClasses(o.Task), class) {
			return fmt.Errorf("unknown evidence class %q", class)
		}
	}
	return nil
}

func adopt(e Example, p Prediction, threshold float64) bool {
	if e.Task == llm.TaskTopicShortlist {
		return p.Shortlist != nil && len(p.Shortlist.Selected) > 0 && p.Confidence != nil && *p.Confidence >= threshold
	}
	if p.Confidence == nil || *p.Confidence < threshold || p.Label == "" || p.Label == "abstain" {
		return false
	}
	if e.Task == llm.TaskTopicMatching && (p.Label == "none_fit" || p.Label == "new") {
		return false
	}
	return true
}

func effectivePredictions(es []Example, b, j map[string]Prediction, threshold float64) (map[string]Prediction, map[string]int, int) {
	out := map[string]Prediction{}
	counts := map[string]int{}
	n := 0
	for _, e := range es {
		out[e.ID] = b[e.ID]
		if adopt(e, j[e.ID], threshold) {
			out[e.ID] = j[e.ID]
			counts[exampleClass(e)]++
			n++
		}
	}
	return out, counts, n
}

func missedLimits(m JudgmentMetrics, limits map[string]float64) []string {
	failed := []string{}
	rates := metricRates(m)
	for key, limit := range limits {
		r := rates[key]
		if r == nil || *r > limit {
			failed = append(failed, key)
		}
	}
	slices.Sort(failed)
	return failed
}

// Qualify selects from confidence boundaries on tuning examples only. It
// minimizes effective errors among thresholds meeting the tuning limits, then
// maximizes adoption, then chooses the higher threshold. If tuning has no
// feasible threshold, the least-error candidate still gets one held-out check.
// Held-out labels never participate in threshold selection.
func Qualify(c Corpus, ps Predictions, o QualificationOptions) (Qualification, error) {
	if err := validateQualification(o); err != nil {
		return Qualification{}, err
	}
	if err := c.Validate(); err != nil {
		return Qualification{}, err
	}
	b, err := predictionIndex(c, ps.Baseline)
	if err != nil {
		return Qualification{}, err
	}
	j, err := predictionIndex(c, ps.Jev)
	if err != nil {
		return Qualification{}, err
	}
	var tuning, held []Example
	for _, e := range c.Examples {
		if e.Task != o.Task {
			continue
		}
		bp, bok := b[e.ID]
		jp, jok := j[e.ID]
		if !bok || !jok || !usablePrediction(e, bp) {
			return Qualification{}, fmt.Errorf("qualification requires complete baseline and candidate predictions")
		}
		if jp.ModelVersion != o.ModelVersion {
			return Qualification{}, fmt.Errorf("candidate model version mismatch")
		}
		if usablePrediction(e, jp) && jp.Confidence == nil {
			return Qualification{}, fmt.Errorf("candidate confidence is unknown")
		}
		if e.Split == "tuning" {
			tuning = append(tuning, e)
		} else {
			held = append(held, e)
		}
	}
	if len(held) == 0 {
		return Qualification{}, fmt.Errorf("qualification requires held-out labels")
	}
	if len(tuning) == 0 {
		return Qualification{}, fmt.Errorf("qualification requires tuning labels")
	}
	thresholds := []float64{0, 1}
	for _, e := range tuning {
		if p := j[e.ID].Confidence; p != nil {
			thresholds = append(thresholds, *p)
			if next := math.Nextafter(*p, 1); next <= 1 {
				thresholds = append(thresholds, next)
			}
		}
	}
	slices.Sort(thresholds)
	thresholds = slices.Compact(thresholds)
	var best JudgmentMetrics
	var threshold float64
	bestAdopted := -1
	bestFeasible := false
	for _, t := range thresholds {
		effective, _, adopted := effectivePredictions(tuning, b, j, t)
		m := scoreJudgments(o.Task, tuning, effective)
		feasible := len(missedLimits(m, o.Limits)) == 0
		if bestAdopted < 0 || feasible && !bestFeasible || feasible == bestFeasible && (m.Errors.Count < best.Errors.Count || m.Errors.Count == best.Errors.Count && (adopted > bestAdopted || adopted == bestAdopted && t > threshold)) {
			best, bestAdopted, bestFeasible, threshold = m, adopted, feasible, t
		}
	}
	effective, adopted, _ := effectivePredictions(held, b, j, threshold)
	out := Qualification{Status: "qualified", Task: o.Task, ModelVersion: o.ModelVersion, Threshold: threshold, Tuning: best, HeldOut: scoreJudgments(o.Task, held, effective), HeldOutPerClass: map[string]int{}, AdoptedPerClass: adopted, Limits: o.Limits, MinimumPerClass: o.MinimumPerClass, FailedLimits: []string{}, InsufficientClasses: []string{}}
	for _, class := range taskClasses(o.Task) {
		out.HeldOutPerClass[class] = 0
		if _, ok := out.AdoptedPerClass[class]; !ok {
			out.AdoptedPerClass[class] = 0
		}
	}
	for _, e := range held {
		out.HeldOutPerClass[exampleClass(e)]++
	}
	for _, class := range taskClasses(o.Task) {
		if out.HeldOutPerClass[class] < o.MinimumPerClass[class] {
			out.InsufficientClasses = append(out.InsufficientClasses, class)
		}
		// Only topic_matching none_fit is never adopted. Shortlist terminal stops
		// qualify the whole nonempty selection and need adopted evidence too.
		if (o.Task != llm.TaskTopicMatching || class != "none_fit") && adopted[class] < o.MinimumPerClass[class] {
			out.InsufficientClasses = append(out.InsufficientClasses, "adopted:"+class)
		}
	}
	rates := metricRates(out.HeldOut)
	for key := range o.Limits {
		if rates[key] == nil {
			out.InsufficientClasses = append(out.InsufficientClasses, "measure:"+key)
		}
	}
	out.FailedLimits = missedLimits(out.HeldOut, o.Limits)
	if len(out.InsufficientClasses) > 0 {
		out.Status = "insufficient_evidence"
		return out, nil
	}
	if len(out.FailedLimits) > 0 {
		out.Status = "failed_qualification"
		return out, nil
	}
	policy := QualifiedPolicy{Name: o.Name, ModelVersion: o.ModelVersion, Threshold: threshold}
	cfg := llm.JudgmentConfig{Policies: map[llm.JudgmentTask]llm.AcceptancePolicy{o.Task: {Name: policy.Name, ModelVersion: policy.ModelVersion, Threshold: policy.Threshold}}}
	if err := cfg.Validate(); err != nil {
		return Qualification{}, fmt.Errorf("validate qualified policy: %w", err)
	}
	out.Policies = map[llm.JudgmentTask]QualifiedPolicy{o.Task: policy}
	return out, nil
}
