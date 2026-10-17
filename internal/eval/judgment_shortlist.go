package eval

import (
	"slices"

	"github.com/kpenfound/hearsay/internal/llm"
)

// ShortlistCapacity is the assertion worker's maximum prompt shortlist size.
// The policy-to-worker regression checks this contract against MaxCandidates.
const ShortlistCapacity = 5

// Shortlist describes a complete selection, in prompt order, and why it ended.
// End is none_fit, capacity, or pool_exhausted. Gold membership is set-valued;
// order is retained for replay but is not adjudicated. For predictions,
// Confidence is the minimum across ALL slot answers, including terminal
// none_fit, and ModelVersion must identify every answer in that sequence.
// Any unusable slot makes the entire prediction abstain, retaining the baseline.
type Shortlist struct {
	Selected []string `json:"selected"`
	End      string   `json:"end"`
}

func validShortlist(e Example, s *Shortlist) bool {
	if s == nil || s.Selected == nil || len(s.Selected) > ShortlistCapacity || len(e.Candidates) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range s.Selected {
		if seen[id] || !slices.Contains(e.Candidates, id) {
			return false
		}
		seen[id] = true
	}
	switch s.End {
	case "none_fit":
		return len(s.Selected) < ShortlistCapacity && len(s.Selected) < len(e.Candidates)
	case "capacity":
		return len(s.Selected) == ShortlistCapacity
	case "pool_exhausted":
		return len(s.Selected) == len(e.Candidates) && len(s.Selected) < ShortlistCapacity
	}
	return false
}

func validExampleLabel(e Example) bool {
	if e.Task == llm.TaskTopicShortlist {
		return e.Label == "" && validShortlist(e, e.Shortlist)
	}
	return e.Shortlist == nil && validLabel(e, e.Label, false)
}

func validPrediction(e Example, p Prediction) bool {
	if e.Task == llm.TaskTopicShortlist {
		return p.Label == "" && validShortlist(e, p.Shortlist) || p.Shortlist == nil && (p.Label == "" || p.Label == "abstain")
	}
	return p.Shortlist == nil && validLabel(e, p.Label, true)
}

func usablePrediction(e Example, p Prediction) bool {
	if e.Task == llm.TaskTopicShortlist {
		return p.Shortlist != nil
	}
	return p.Label != "" && p.Label != "abstain"
}

func sameShortlist(a, b *Shortlist) bool {
	if a == nil || b == nil || a.End != b.End || len(a.Selected) != len(b.Selected) {
		return false
	}
	for _, id := range a.Selected {
		if !slices.Contains(b.Selected, id) {
			return false
		}
	}
	return true
}
