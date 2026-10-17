package jev

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/kpenfound/hearsay/internal/llm"
)

// System One allows rounded distributions within this tolerance. Accepted
// distributions are normalized for the shared capability's strict contract.
const distributionTolerance = 0.02

type wireAnswer struct {
	Type          string              `json:"type"`
	Choice        *string             `json:"choice"`
	Score         *float64            `json:"score"`
	Noul          *float64            `json:"noul"`
	Probabilities map[string]*float64 `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
}

func decode(data []byte, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	var wire struct{ Model, Answers, Usage json.RawMessage }
	if json.Unmarshal(data, &wire) != nil {
		return llm.JudgmentResponse{}, llm.ErrJudgmentResponse
	}
	out := llm.JudgmentResponse{}
	var valid bool
	out.Usage, valid = reportedUsage(wire.Usage)
	// Decode accounting independently: a wrong model/answer type must not
	// erase otherwise valid partial usage. Invalid JSON is never salvaged.
	if json.Unmarshal(wire.Model, &out.ModelVersion) != nil || strings.TrimSpace(out.ModelVersion) == "" || len(out.ModelVersion) > 128 {
		out.ModelVersion = ""
		return out, llm.ErrJudgmentResponse
	}
	if !valid {
		return out, llm.ErrJudgmentResponse
	}
	var answers map[string]json.RawMessage
	if json.Unmarshal(wire.Answers, &answers) != nil || len(answers) != len(req.Questions) {
		return out, llm.ErrJudgmentResponse
	}
	decoded := make([]llm.JudgmentAnswer, 0, len(req.Questions))
	for _, q := range req.Questions {
		var a wireAnswer
		if json.Unmarshal(answers[q.ID], &a) != nil {
			return out, llm.ErrJudgmentResponse
		}
		answer, ok := checkAnswer(q, a)
		if !ok {
			return out, llm.ErrJudgmentResponse
		}
		decoded = append(decoded, answer)
	}
	out.Answers = decoded
	if !llm.ValidJudgmentResponse(req, out) {
		out.Answers = nil
		return out, llm.ErrJudgmentResponse
	}
	return out, nil
}

func unit(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 1 }

func checkAnswer(q llm.JudgmentQuestion, a wireAnswer) (llm.JudgmentAnswer, bool) {
	out := llm.JudgmentAnswer{ID: q.ID}
	if q.Kind == llm.JudgmentYesNo {
		if a.Type != "noul" || a.Noul == nil || !unit(*a.Noul) || a.Choice != nil || a.Score != nil {
			return out, false
		}
		p := *a.Noul
		yes := p >= 0.5
		out.Yes = &yes
		out.Probabilities = map[string]float64{"yes": p, "no": 1 - p}
		// Noul has no vendor confidence field. The larger of the two
		// probabilities is the confidence in the selected Boolean answer.
		out.Confidence = math.Max(p, 1-p)
		return out, true
	}
	if a.Confidence == nil || !unit(*a.Confidence) || a.Noul != nil {
		return out, false
	}
	opts := llm.JudgmentOptions(q)
	if len(a.Probabilities) != len(opts) {
		return out, false
	}
	out.Probabilities = make(map[string]float64, len(opts))
	sum := 0.0
	for i, label := range opts {
		key := label
		if q.Kind == llm.JudgmentScore {
			key = strconv.Itoa(i)
		}
		p := a.Probabilities[key]
		// Pointers distinguish explicit numeric zero from JSON null/missing.
		if p == nil || !unit(*p) {
			return out, false
		}
		out.Probabilities[label] = *p
		sum += *p
	}
	if math.Abs(sum-1) > distributionTolerance {
		return out, false
	}
	for key, p := range out.Probabilities {
		out.Probabilities[key] = p / sum
	}
	out.Confidence = *a.Confidence
	switch q.Kind {
	case llm.JudgmentChoice:
		if a.Type != "choice" || a.Choice == nil || a.Score != nil {
			return out, false
		}
		out.Choice = *a.Choice
	case llm.JudgmentScore:
		if a.Type != "score" || a.Score == nil || a.Choice != nil || math.IsNaN(*a.Score) || *a.Score < 0 || *a.Score > float64(q.Max-q.Min) {
			return out, false
		}
		score := *a.Score + float64(q.Min)
		out.Score = &score
	default:
		return out, false
	}
	return out, true
}

func reportedUsage(data json.RawMessage) (llm.JudgmentUsage, bool) {
	out := llm.JudgmentUsage{}
	if len(data) == 0 {
		return out, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return out, false
	}
	valid := true
	for _, f := range []struct {
		name   string
		target **int64
	}{{"input_tokens", &out.InputTokens}, {"output_tokens", &out.OutputTokens}} {
		if raw, ok := fields[f.name]; ok {
			var n *int64
			if json.Unmarshal(raw, &n) != nil || n == nil || *n < 0 {
				valid = false
			} else {
				*f.target = n
			}
		}
	}
	if raw, ok := fields["cost"]; ok {
		var n *float64
		if json.Unmarshal(raw, &n) != nil || n == nil || math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 {
			valid = false
		} else {
			out.CostUSD = n
		}
	}
	return out, valid
}
