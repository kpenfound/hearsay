//go:build integration

package distiller_test

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

type outcomeJev struct {
	t        *testing.T
	mu       sync.Mutex
	labels   map[string]string
	fallback string
	asked    []llm.JudgmentRequest
	fail     error
}

func (o *outcomeJev) byDefault(label string) *outcomeJev {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fallback = label
	return o
}

func newOutcomeJev(t *testing.T) *outcomeJev {
	t.Helper()
	return &outcomeJev{t: t, labels: map[string]string{}}
}

func (o *outcomeJev) classify(doc l1.Document, label string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.labels[distiller.OutcomeRequest(doc, llm.JudgmentConfig{}).State] = label
}

func (o *outcomeJev) answer(doc l1.Document, answer map[string]any) json.RawMessage {
	o.t.Helper()
	body, kind := modelAnswer(o.t, answer)
	o.classify(doc, kind)
	return body
}

func (o *outcomeJev) failWith(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail = err
}

func (o *outcomeJev) Calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.asked)
}

func (o *outcomeJev) Requests() []llm.JudgmentRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]llm.JudgmentRequest(nil), o.asked...)
}

func (o *outcomeJev) Judge(_ context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	o.mu.Lock()
	o.asked = append(o.asked, req)
	label, ok := o.labels[req.State]
	if !ok {
		label = o.fallback
	}
	fail := o.fail
	o.mu.Unlock()
	if fail != nil {
		return llm.JudgmentResponse{}, fail
	}
	return outcomeAnswers(o.t, req, label), nil
}

func (o *outcomeJev) judgments() llm.Judgments {
	o.t.Helper()
	return jevOver(o.t, o, llm.JudgmentConfig{})
}

func jevOver(t *testing.T, provider llm.JudgmentProvider, cfg llm.JudgmentConfig) llm.Judgments {
	t.Helper()
	cfg.Model = "recorded-v1"
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = -1
	}
	j, err := llm.NewJudgments(cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return provider }, func(string) string { return "fake-only" })
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func fixtureJev(t *testing.T) *outcomeJev {
	t.Helper()
	o := newOutcomeJev(t)
	for _, state := range fixtureStates() {
		for _, doc := range documentsIn(t, state.events) {
			answer, ok := state.answers[doc.ID]
			if !ok {
				answer = answers[doc.ID]
			}
			_, kind := modelAnswer(t, answer)
			o.classify(doc, kind)
		}
	}
	return o
}

var outcomeYes = map[string]string{"resolved": "work_finished", "decided": "binding_agreement", "proposed": "unagreed_proposal", "open": "still_open"}

func outcomeAnswers(t *testing.T, req llm.JudgmentRequest, label string) llm.JudgmentResponse {
	t.Helper()
	raw, err := os.ReadFile("testdata/outcome-judgments/recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		ModelVersion string                        `json:"model_version"`
		Usage        llm.JudgmentUsage             `json:"usage"`
		Answers      map[string]llm.JudgmentAnswer `json:"answers"`
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	out := llm.JudgmentResponse{ModelVersion: f.ModelVersion, Usage: f.Usage}
	for _, q := range req.Questions {
		atom := "no"
		switch {
		case label == "uncertain" && q.ID == "binding_agreement":
			atom = "uncertain"
		case outcomeYes[label] == q.ID:
			atom = "yes"
		}
		a := f.Answers[atom]
		a.ID = q.ID
		out.Answers = append(out.Answers, a)
	}
	return out
}
