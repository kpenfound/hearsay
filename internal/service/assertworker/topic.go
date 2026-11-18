package assertworker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
)

// sameQuestionVersion is the topic.same_question variant journaled. v2 asks
// it of every position, over the candidates no earlier position of the
// document took.
const sameQuestionVersion = "topic-same-question-v2"

// TopicSameQuestionRequest is the provider request [sameQuestionQuestions] makes.
func TopicSameQuestionRequest(name, position string, candidates []Candidate) llm.JudgmentRequest {
	return toRequest(sameQuestionQuestions(name, position, candidates))
}

// sameQuestionQuestions asks, once per candidate, whether a position answers
// the same question as that topic, rather than sharing subject matter.
// Candidate text is already filtered for the audience. Each question is judged
// on its own; code decides which, if any, the position continues.
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

// placement is where code puts one position: on candidate at, or a new topic
// when at is -1, with the judgement against the position it was compared to.
type placement struct {
	index          int // the position's place in the model's answer
	at             int
	name, position string
	judgement      l2.Judgement
}

// place decides where each position the model extracted goes. Per position,
// Jev is asked topic.same_question of every candidate no earlier position of
// this document took: the confident yes most likely to be yes wins (the first,
// on a tie), and no confident yes opens a new topic. A position on a topic
// whose current position its readers may read is then compared to it
// (stance.same_answer).
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
				// A new topic needs a question to be named by.
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
