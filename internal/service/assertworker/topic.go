package assertworker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
)

const sameQuestionVersion = "topic-same-question-v2"

// TopicSameQuestionRequest is the provider request [sameQuestionQuestions] makes.
func TopicSameQuestionRequest(name, position string, candidates []Candidate) llm.JudgmentRequest {
	return toRequest(sameQuestionQuestions(name, position, candidates))
}

func sameQuestionQuestions(name, position string, candidates []Candidate) (string, []yesNo) {
	type offered struct{ Label, Name, Current string }
	state := struct {
		Evidence   struct{ Name, Position string }
		Candidates []offered
	}{}
	state.Evidence.Name, state.Evidence.Position = clean(name), clean(position)
	questions := make([]yesNo, 0, len(candidates))
	for i, c := range candidates {
		state.Candidates = append(state.Candidates, offered{label(i), clean(c.Name), clean(c.Current)})
		questions = append(questions, yesNo{Task: llm.TaskTopicSameQuestion, Version: sameQuestionVersion, ID: label(i),
			Question: guard + fmt.Sprintf("Does the assertion in Evidence answer the same question as topic %s? Sharing a subject or reference is insufficient.", label(i))})
	}
	raw, _ := json.Marshal(state)
	return string(raw), questions
}

type placement struct {
	index          int
	at             int
	name, position string
	judgement      l2.Judgement
}

func (a *Asserter) place(ctx context.Context, ans answer, p preparedAssertion, batch *judgmentBatch) ([]placement, error) {
	taken := map[int]bool{}
	var out []placement
	for i, as := range ans.Assertions {
		pl := placement{index: i, at: -1, name: clean(as.TopicName), position: clean(as.Position), judgement: l2.JudgementUnknown}
		if pl.position == "" {
			continue
		}
		var offered []int
		var shown []Candidate
		for j, c := range p.candidates {
			if !taken[j] {
				offered, shown = append(offered, j), append(shown, c)
			}
		}
		if len(offered) > 0 {
			state, questions := sameQuestionQuestions(pl.name, pl.position, shown)
			records, err := a.evaluateJudgments(ctx, state, questions, p, batch)
			if err != nil {
				return nil, err
			}
			best := -1
			for k, r := range records {
				if a.yes(r) && (best < 0 || r.Probabilities["yes"] > records[best].Probabilities["yes"]) {
					best = k
				}
			}
			if best >= 0 {
				pl.at = offered[best]
			}
		}
		if pl.at < 0 {
			if pl.name == "" {
				continue
			}
		} else {
			taken[pl.at] = true
			if current := p.candidates[pl.at].Current; current != "" {
				judgement, err := a.sameAnswer(ctx, p.topics[pl.at].Name, current, pl.position, p, batch)
				if err != nil {
					return nil, err
				}
				pl.judgement = judgement
			}
		}
		out = append(out, pl)
	}
	return out, nil
}
