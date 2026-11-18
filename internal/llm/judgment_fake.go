package llm

import (
	"context"
	"sync"
	"time"
)

// JudgmentStep scripts one fake attempt: slow answers honor cancellation;
// Error can be a retryable Error (including 429/529 and RetryAfter), a terminal
// failure or a wrapped ErrJudgmentResponse. Response may deliberately be invalid.
type JudgmentStep struct {
	Delay    time.Duration
	Response JudgmentResponse
	Error    error
}

// FakeJudgmentProvider replays scripted attempts, without any live-provider
// fallback. After the last step it returns ErrNoFixture. It is safe to share.
type FakeJudgmentProvider struct {
	mu    sync.Mutex
	steps []JudgmentStep
	calls []JudgmentRequest
}

// NewFakeJudgmentProvider constructs a script for a real NewJudgments wrapper.
func NewFakeJudgmentProvider(steps ...JudgmentStep) *FakeJudgmentProvider {
	return &FakeJudgmentProvider{steps: steps}
}

// Calls reports attempts, including ones canceled while waiting.
func (f *FakeJudgmentProvider) Calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// Requests returns a snapshot for checking question construction in worker tests.
func (f *FakeJudgmentProvider) Requests() []JudgmentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]JudgmentRequest, len(f.calls))
	for i, r := range f.calls {
		out[i] = cloneJudgmentRequest(r)
	}
	return out
}

// Judge consumes one step, returning sanitized fixture misses.
func (f *FakeJudgmentProvider) Judge(ctx context.Context, req JudgmentRequest) (JudgmentResponse, error) {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, cloneJudgmentRequest(req))
	if n >= len(f.steps) {
		f.mu.Unlock()
		return JudgmentResponse{}, ErrNoFixture
	}
	step := f.steps[n]
	f.mu.Unlock()
	if step.Delay > 0 {
		timer := time.NewTimer(step.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return JudgmentResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	return step.Response, step.Error
}

func cloneJudgmentRequest(r JudgmentRequest) JudgmentRequest {
	out := r
	out.Questions = append([]JudgmentQuestion(nil), r.Questions...)
	for i := range out.Questions {
		out.Questions[i].Options = append([]string(nil), r.Questions[i].Options...)
	}
	return out
}

// FakeJudgments is a Jev for tests in other packages: yes answers every yes/no
// question with the probability of yes, given the request's state and the
// question, and the answer's confidence is the larger of the two
// probabilities. It never calls a provider.
func FakeJudgments(yes func(state string, q JudgmentQuestion) float64) Judgments {
	j, _ := NewJudgments(JudgmentConfig{Model: "fake-jev"}, func(JudgmentConfig, string) JudgmentProvider { return yesNoProvider(yes) }, func(string) string { return "fake" })
	return j
}

type yesNoProvider func(state string, q JudgmentQuestion) float64

func (f yesNoProvider) Judge(_ context.Context, req JudgmentRequest) (JudgmentResponse, error) {
	zero, cost := int64(0), float64(0)
	out := JudgmentResponse{ModelVersion: "fake-jev", Usage: JudgmentUsage{InputTokens: &zero, OutputTokens: &zero, CostUSD: &cost}}
	for _, q := range req.Questions {
		p := f(req.State, q)
		yes := p >= 0.5
		out.Answers = append(out.Answers, JudgmentAnswer{ID: q.ID, Yes: &yes, Confidence: max(p, 1-p), Probabilities: map[string]float64{"yes": p, "no": 1 - p}})
	}
	return out, nil
}
