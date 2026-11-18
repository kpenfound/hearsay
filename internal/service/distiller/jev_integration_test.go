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

// outcomeJev is the fake Jev the integration tests distil with. It answers the
// outcome questions about a document the way the test classified it: yes to
// the question that kind's yes answers, no to the others, from the recorded
// atoms in testdata/outcome-judgments. A document nobody classified takes the
// default, which is none unless [outcomeJev.byDefault] says otherwise. It
// never reaches a network.
type outcomeJev struct {
	t        *testing.T
	mu       sync.Mutex
	labels   map[string]string
	fallback string
	asked    []llm.JudgmentRequest
	fail     error
}

// byDefault classifies every document nobody classified as label.
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

// classify says how doc's outcome is to be judged: an outcome kind, or
// "uncertain" for a yes below any threshold on the binding agreement question.
func (o *outcomeJev) classify(doc l1.Document, label string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.labels[distiller.OutcomeRequest(doc).State] = label
}

// answer is the model's answer for doc, with the test's outcome_kind taken out
// of it and given to Jev instead: the model no longer classifies.
func (o *outcomeJev) answer(doc l1.Document, answer map[string]any) json.RawMessage {
	o.t.Helper()
	body, kind := modelAnswer(o.t, answer)
	o.classify(doc, kind)
	return body
}

// failWith makes every later call fail with err.
func (o *outcomeJev) failWith(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fail = err
}

// Calls is how many requests Jev was sent.
func (o *outcomeJev) Calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.asked)
}

// Requests is what Jev was sent.
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

// judgments is Jev as the distiller takes it.
func (o *outcomeJev) judgments() llm.Judgments {
	o.t.Helper()
	return jevOver(o.t, o, llm.JudgmentConfig{})
}

// jevOver wraps a fake provider in the real capability, retries off.
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

// fixtureJev is Jev for the fixture repository: every document of every state
// classified as the answers written down for it say.
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

// outcomeYes is the question a yes answers for each outcome; none has none.
var outcomeYes = map[string]string{"resolved": "work_finished", "decided": "binding_agreement", "proposed": "unagreed_proposal", "open": "still_open"}

// outcomeAnswers answers the questions req asks with recorded atoms, so that
// code decides label: its question yes, every other no. "uncertain" answers the
// binding_agreement question yes below any threshold.
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
