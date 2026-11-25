//go:build integration

package assertworker_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

const jevModel = "jev-fixture-v1"

type shownCandidate struct{ Label, Name, Current string }

type ruleJev struct {
	sameQuestion func(name, position string, c shownCandidate) float64
	sameAnswer   func(previous, position string) float64
	outcome      map[string]float64
	fail         error

	mu       sync.Mutex
	requests []llm.JudgmentRequest
}

func newRuleJev() *ruleJev {
	return &ruleJev{
		sameQuestion: func(name, _ string, c shownCandidate) float64 { return likely(strings.EqualFold(name, c.Name)) },
		sameAnswer:   func(previous, position string) float64 { return likely(previous == position) },
		outcome:      map[string]float64{"unagreed_proposal": likely(true)},
	}
}

func likely(yes bool) float64 {
	if yes {
		return 0.96
	}
	return 0.04
}

func (j *ruleJev) Judge(_ context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	j.mu.Lock()
	j.requests = append(j.requests, req)
	j.mu.Unlock()
	if j.fail != nil {
		return llm.JudgmentResponse{}, j.fail
	}
	p := map[string]float64{}
	switch judgmentKind(req) {
	case "unknown":
		p = j.outcome
	case "same_answer":
		var state struct{ Previous, Position string }
		_ = json.Unmarshal([]byte(req.State), &state)
		p["same_answer"] = j.sameAnswer(state.Previous, state.Position)
	case "same_question":
		var state struct {
			Evidence   struct{ Name, Position string }
			Candidates []shownCandidate
		}
		_ = json.Unmarshal([]byte(req.State), &state)
		for _, c := range state.Candidates {
			p[c.Label] = j.sameQuestion(state.Evidence.Name, state.Evidence.Position, c)
		}
	}
	zero, input, cost := int64(0), int64(40), float64(0)
	out := llm.JudgmentResponse{ModelVersion: jevModel, Usage: llm.JudgmentUsage{InputTokens: &input, OutputTokens: &zero, CostUSD: &cost}}
	for _, q := range req.Questions {
		yes := p[q.ID]
		answer := yes >= 0.5
		confidence := yes
		if !answer {
			confidence = 1 - yes
		}
		out.Answers = append(out.Answers, llm.JudgmentAnswer{ID: q.ID, Yes: &answer, Confidence: confidence,
			Probabilities: map[string]float64{"yes": yes, "no": 1 - yes}})
	}
	return out, nil
}

func (j *ruleJev) asked(kind string) []llm.JudgmentRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []llm.JudgmentRequest
	for _, r := range j.requests {
		if judgmentKind(r) == kind {
			out = append(out, r)
		}
	}
	return out
}

func (j *ruleJev) calls() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.requests)
}

func (j *ruleJev) judgments(t *testing.T) llm.Judgments {
	t.Helper()
	capability, err := llm.NewJudgments(llm.JudgmentConfig{Model: jevModel, MaxRetries: -1},
		func(llm.JudgmentConfig, string) llm.JudgmentProvider { return j }, func(string) string { return "fixture-key" })
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

func judgmentKind(req llm.JudgmentRequest) string {
	switch q := req.Questions[0]; {
	case q.ID == "same_answer":
		return "same_answer"
	case strings.Contains(q.Question, "same question as topic"):
		return "same_question"
	}
	return "unknown"
}

func shown(t *testing.T, req llm.JudgmentRequest) []shownCandidate {
	t.Helper()
	var state struct{ Candidates []shownCandidate }
	if err := json.Unmarshal([]byte(req.State), &state); err != nil {
		t.Fatal(err)
	}
	return state.Candidates
}

func judgmentReader() l1.Reader {
	return l1.Reader{Effective: principal.Effective{Human: "kyle", Grant: principal.Grant{Scopes: principal.AllScopes()}}}
}

func newJevAsserter(t *testing.T, pool *pgxpool.Pool, src string, fx *llm.Fixtures, j *ruleJev) *assertworker.Asserter {
	t.Helper()
	registry, err := llm.NewFake(llm.Default(), fx)
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	a, err := assertworker.New(pool, registry, testConfig(src))
	if err != nil {
		t.Fatalf("assertworker.New() = %v", err)
	}
	return a.WithJudgments(j.judgments(t))
}

func writtenJudgment(t *testing.T, pool *pgxpool.Pool, doc string, want l2.Judgement) {
	t.Helper()
	stances, err := l2.New(pool).StancesFrom(t.Context(), doc)
	if err != nil || len(stances) != 1 || stances[0].Judgement != want {
		t.Fatalf("stances = %+v, %v; want one judged %s", stances, err, want)
	}
}
