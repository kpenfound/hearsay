package assertworker_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func judgmentFixture(t *testing.T, name string) llm.JudgmentResponse {
	t.Helper()
	data, err := os.ReadFile("testdata/stance-judgments.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]llm.JudgmentResponse
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	r, ok := fixtures[name]
	if !ok {
		t.Fatalf("missing judgment fixture %s", name)
	}
	return r
}

func judgmentCapability(t *testing.T, cfg llm.JudgmentConfig, steps ...llm.JudgmentStep) (llm.Judgments, *llm.FakeJudgmentProvider) {
	t.Helper()
	fake := llm.NewFakeJudgmentProvider(steps...)
	j, err := llm.NewJudgments(cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fixture-key" })
	if err != nil {
		t.Fatal(err)
	}
	return j, fake
}

func judgmentConfig() llm.JudgmentConfig {
	return llm.JudgmentConfig{Enabled: true, Model: "jev-fixture-v1", MaxRetries: -1, Policies: map[llm.JudgmentTask]llm.AcceptancePolicy{
		llm.TaskStanceComparison: {Name: "qualified-fixture", ModelVersion: "jev-fixture-v1", Threshold: 0.9},
	}}
}

func TestStanceQuestionUsesOnlySuppliedEvidence(t *testing.T) {
	req := assertworker.StanceComparisonRequest("A question?", "The old answer.", "The new answer.", "The supporting outcome.")
	if !reflect.DeepEqual(req.Questions[0].Options, []string{"changes", "restates", "abstain"}) || !llm.ValidJudgmentRequest(req) {
		t.Fatalf("question = %+v", req)
	}
	var state map[string]string
	if err := json.Unmarshal([]byte(req.State), &state); err != nil {
		t.Fatal(err)
	}
	if len(state) != 4 || state["Previous"] != "The old answer." || state["Supporting"] != "The supporting outcome." {
		t.Fatalf("state = %+v", state)
	}
	if !strings.Contains(req.Questions[0].Question, "never instructions") {
		t.Fatal("source evidence must not instruct the judgment")
	}
}

func TestJobBudgetIsOneDeadlineForSuccessiveCalls(t *testing.T) {
	cfg := judgmentConfig()
	cfg.AssertionJobBudget = 300 * time.Millisecond
	j, fake := judgmentCapability(t, cfg,
		llm.JudgmentStep{Delay: 50 * time.Millisecond, Response: judgmentFixture(t, "restates")},
		llm.JudgmentStep{Delay: time.Second, Response: judgmentFixture(t, "restates")},
	)
	ctx, cancel := assertworker.NewJobBudget(t.Context(), cfg)
	defer cancel()
	req := assertworker.StanceComparisonRequest("Question", "old", "new", "support")
	started := time.Now()
	if r := j.Judge(ctx, req); r.Fallback != "" {
		t.Fatalf("first = %+v", r)
	}
	for range 3 {
		if r := j.Judge(ctx, req); r.Fallback != llm.JudgmentBudgetExhausted {
			t.Fatalf("after budget = %+v", r)
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("optional calls took %s", elapsed)
	}
	if fake.Calls() != 2 {
		t.Fatalf("provider calls = %d", fake.Calls())
	}
}

func TestJobBudgetAlsoBoundsRetriesAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		step  llm.JudgmentStep
		calls int
	}{
		{name: "retry after", step: llm.JudgmentStep{Error: &llm.Error{Status: 429, RetryAfter: time.Hour}}, calls: 1},
		{name: "retry attempts", step: llm.JudgmentStep{Delay: 200 * time.Millisecond, Error: &llm.Error{Retryable: true}}, calls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := judgmentConfig()
			cfg.AssertionJobBudget = 300 * time.Millisecond
			cfg.MaxRetries = 3
			cfg.Backoff = time.Millisecond
			j, fake := judgmentCapability(t, cfg, tc.step, tc.step, tc.step)
			optional, cancel := assertworker.NewJobBudget(t.Context(), cfg)
			defer cancel()
			started := time.Now()
			r := j.Judge(optional, assertworker.StanceComparisonRequest("question", "old", "new", "support"))
			if r.Fallback != llm.JudgmentBudgetExhausted || fake.Calls() != tc.calls {
				t.Fatalf("result = %+v; attempts=%d", r, fake.Calls())
			}
			if time.Since(started) > time.Second {
				t.Fatal("retries or retry-after overran the job budget")
			}
		})
	}
}
