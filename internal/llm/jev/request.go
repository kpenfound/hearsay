package jev

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/kpenfound/hearsay/internal/llm"
)

type wireQuestion struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

func requestBody(cfg llm.JudgmentConfig, r llm.JudgmentRequest) ([]byte, error) {
	if len(r.State) > cfg.MaxStateQuestionTokens || len(cfg.Model) > cfg.MaxTotalTokens {
		return nil, llm.ErrJudgmentOversize
	}
	questions := make(map[string]wireQuestion, len(r.Questions))
	state, _ := json.Marshal(r.State)
	total := len(state) + len(cfg.Model)
	for _, q := range r.Questions {
		// Refuse giant raw strings before allocating their escaped JSON.
		rawSize := len(r.State)
		for _, part := range append([]string{q.ID, q.Question, q.Rubric}, q.Options...) {
			if len(part) > cfg.MaxStateQuestionTokens-rawSize {
				return nil, llm.ErrJudgmentOversize
			}
			rawSize += len(part)
		}
		w := wireQuestion{Instructions: q.Question}
		switch q.Kind {
		case llm.JudgmentChoice:
			w.Type = "choice"
			// The order of choice criteria is significant to System One.
			var b bytes.Buffer
			b.WriteByte('{')
			for i, option := range q.Options {
				if i > 0 {
					b.WriteByte(',')
				}
				label, _ := json.Marshal(option)
				b.Write(label)
				b.WriteByte(':')
				b.Write(label)
			}
			b.WriteByte('}')
			w.Criteria = b.Bytes()
		case llm.JudgmentScore:
			w.Type = "score"
			w.Instructions += "\n" + q.Rubric
			levels := make([]string, 0, q.Max-q.Min+1)
			for n := q.Min; n <= q.Max; n++ {
				levels = append(levels, strconv.Itoa(n))
			}
			w.Criteria, _ = json.Marshal(levels)
		case llm.JudgmentYesNo:
			w.Type = "noul"
		}
		encoded, _ := json.Marshal(w)
		// UTF-8 bytes of serialized JSON conservatively bound model tokens,
		// including escaped text and schema overhead. This may refuse some
		// requests a vendor tokenizer would admit, but never guesses a ratio.
		if len(state)+len(encoded)+len(q.ID)+32 > cfg.MaxStateQuestionTokens {
			return nil, llm.ErrJudgmentOversize
		}
		total += len(encoded) + len(q.ID) + 4
		if total > cfg.MaxTotalTokens {
			return nil, llm.ErrJudgmentOversize
		}
		questions[q.ID] = w
	}
	body, err := json.Marshal(struct {
		State     string                  `json:"state"`
		Model     string                  `json:"model"`
		Questions map[string]wireQuestion `json:"questions"`
	}{r.State, cfg.Model, questions})
	if err != nil {
		return nil, failure("invalid_request", 0, false)
	}
	if len(body) > cfg.MaxTotalTokens {
		return nil, llm.ErrJudgmentOversize
	}
	return body, nil
}
