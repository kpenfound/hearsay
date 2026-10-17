package eval_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
)

func count(n int64) *int64 { return &n }
func usage(input, output int64, cost float64) llm.JudgmentUsage {
	return llm.JudgmentUsage{InputTokens: count(input), OutputTokens: count(output), CostUSD: share(cost)}
}
func observation(cohort string) l1.PipelineObservation {
	return l1.PipelineObservation{Cohort: cohort, HandlerNS: count(int64(time.Second)), QueueDelayNS: count(int64(2 * time.Second)), GenerativeCalls: count(2), GenerativeAttempts: count(3), GenerativeUsage: usage(100, 10, .25), JudgmentRequests: count(1), JudgmentAttempts: count(1), JudgmentFallbacks: count(0), JudgmentUsage: usage(20, 1, .05), CandidateCount: count(5)}
}

func TestPipelineTotals(t *testing.T) {
	a, b, c := observation("baseline"), observation("shadow"), observation("accepted")
	// One rate-limit attempt, then two cooldown suppressions: three logical
	// observations, one provider attempt, all fallback, no known billing.
	c.JudgmentRequests = count(3)
	c.JudgmentAttempts = count(1)
	c.JudgmentFallbacks = count(3)
	c.JudgmentUsage = llm.JudgmentUsage{}
	b.JudgmentAttempts = count(3) // retries do not inflate requests
	report := eval.SummarizePipelines([]l1.PipelineObservation{a, b, c})
	for _, m := range []eval.PipelineCohort{report.Baseline, report.Shadow} {
		if m.Runs != 1 || m.TotalCostUSD == nil || math.Abs(*m.TotalCostUSD-.30) > 1e-12 || *m.GenerativeCalls != 2 || m.GenerativeAttempts == nil || *m.GenerativeAttempts != 3 || *m.CandidateCount != 5 || *m.HandlerSeconds != 1 || *m.QueueDelaySeconds != 2 {
			t.Fatalf("cohort=%+v", m)
		}
	}
	if *report.Shadow.JudgmentRequests != 1 || *report.Shadow.JudgmentAttempts != 3 || *report.Accepted.FallbackFrequency != 1 || report.Accepted.TotalCostUSD != nil {
		t.Fatalf("report=%+v", report)
	}
}

func TestPipelineUnknownAndOverflow(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*l1.PipelineObservation)
		check  func(*testing.T, eval.PipelineCohort)
	}{
		{"unknown usage", func(o *l1.PipelineObservation) { o.JudgmentUsage.InputTokens = nil; o.JudgmentUsage.CostUSD = nil }, func(t *testing.T, c eval.PipelineCohort) {
			if c.JudgmentUsage.InputTokens != nil || c.TotalCostUSD != nil || c.JudgmentUsage.OutputTokens == nil {
				t.Fatal(c)
			}
		}},
		{"token overflow", func(o *l1.PipelineObservation) {
			o.GenerativeUsage.InputTokens = count(math.MaxInt64)
			o.JudgmentUsage.OutputTokens = count(math.MaxInt64)
		}, func(t *testing.T, c eval.PipelineCohort) {
			if c.GenerativeUsage.InputTokens != nil || c.JudgmentUsage.OutputTokens != nil {
				t.Fatal(c)
			}
		}},
		{"cost overflow", func(o *l1.PipelineObservation) {
			o.GenerativeUsage.CostUSD = share(math.MaxFloat64)
			o.JudgmentUsage.CostUSD = share(math.MaxFloat64)
		}, func(t *testing.T, c eval.PipelineCohort) {
			if c.TotalCostUSD != nil || c.GenerativeUsage.CostUSD != nil || c.JudgmentUsage.CostUSD != nil {
				t.Fatal(c)
			}
		}},
		{"count duration overflow", func(o *l1.PipelineObservation) {
			o.GenerativeCalls = count(math.MaxInt64)
			o.GenerativeAttempts = count(math.MaxInt64)
			o.HandlerNS = count(math.MaxInt64)
			o.JudgmentAttempts = count(math.MaxInt64)
			o.CandidateCount = count(math.MaxInt64)
		}, func(t *testing.T, c eval.PipelineCohort) {
			if c.GenerativeCalls != nil || c.GenerativeAttempts != nil || c.HandlerSeconds != nil || c.MeanHandlerSeconds != nil || c.JudgmentAttempts != nil || c.CandidateCount != nil {
				t.Fatal(c)
			}
		}},
		{"unknown delay", func(o *l1.PipelineObservation) { o.QueueDelayNS = nil }, func(t *testing.T, c eval.PipelineCohort) {
			if c.QueueDelaySeconds != nil || c.MeanQueueDelaySeconds == nil || *c.MeanQueueDelaySeconds != 2 || c.MissingQueueDelayRuns != 2 {
				t.Fatal(c)
			}
		}},
		{"nonfinite", func(o *l1.PipelineObservation) { o.JudgmentUsage.CostUSD = share(math.Inf(1)) }, func(t *testing.T, c eval.PipelineCohort) {
			if c.TotalCostUSD != nil || c.JudgmentUsage.CostUSD != nil {
				t.Fatal(c)
			}
		}},
		{"negative", func(o *l1.PipelineObservation) {
			o.JudgmentUsage.InputTokens = count(-1)
			o.JudgmentUsage.CostUSD = share(-1)
		}, func(t *testing.T, c eval.PipelineCohort) {
			if c.JudgmentUsage.InputTokens != nil || c.TotalCostUSD != nil {
				t.Fatal(c)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := observation("shadow")
			tt.mutate(&o)
			report := eval.SummarizePipelines([]l1.PipelineObservation{o, o, observation("shadow")})
			tt.check(t, report.Shadow)
			if _, err := json.Marshal(report); err != nil {
				t.Fatalf("report is not valid JSON: %v", err)
			}
		})
	}
	// A finite cross-provider sum may itself overflow even without per-provider
	// aggregation overflow.
	o := observation("accepted")
	o.GenerativeUsage.CostUSD = share(math.MaxFloat64)
	o.JudgmentUsage.CostUSD = share(math.MaxFloat64)
	if c := eval.SummarizePipelines([]l1.PipelineObservation{o}).Accepted; c.TotalCostUSD != nil {
		t.Fatal(c)
	}
}

func TestPipelineEmptyAndPartialDelay(t *testing.T) {
	r := eval.SummarizePipelines(nil)
	for _, c := range []eval.PipelineCohort{r.Baseline, r.Shadow, r.Accepted} {
		if c.Runs != 0 || c.TotalCostUSD != nil || c.GenerativeCalls != nil || c.HandlerSeconds != nil || c.QueueDelaySeconds != nil || c.FallbackFrequency != nil {
			t.Fatal(c)
		}
	}
	a, b := observation("accepted"), observation("accepted")
	b.QueueDelayNS = nil
	c := eval.SummarizePipelines([]l1.PipelineObservation{a, b}).Accepted
	if c.QueueDelaySeconds != nil || c.MeanQueueDelaySeconds == nil || *c.MeanQueueDelaySeconds != 2 || c.MissingQueueDelayRuns != 1 {
		t.Fatal(c)
	}
}

func TestCooldownFallbackZeroConsumption(t *testing.T) {
	fake := llm.NewFakeJudgmentProvider(llm.JudgmentStep{Error: &llm.Error{Status: 429, RetryAfter: time.Hour}})
	j, err := llm.NewJudgments(llm.JudgmentConfig{Enabled: true, Model: "test-v1", MaxRetries: -1, CooldownFailures: 1}, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fixture-only" })
	if err != nil {
		t.Fatal(err)
	}
	question := llm.JudgmentQuestion{ID: "q", Question: "Fixture only", Options: []string{"yes", "no"}}
	first := j.Choice(t.Context(), "fixture", question)
	if first.Attempts != 1 {
		t.Fatal(first)
	}
	var os []l1.PipelineObservation
	for range 2 {
		r := j.Choice(t.Context(), "fixture", question)
		if r.Fallback != llm.JudgmentCooldown || r.Attempts != 0 {
			t.Fatal(r)
		}
		o := observation("accepted")
		o.JudgmentRequests = count(1)
		o.JudgmentAttempts = count(int64(r.Attempts))
		o.JudgmentFallbacks = count(1)
		o.JudgmentUsage = r.Usage
		os = append(os, o)
	}
	c := eval.SummarizePipelines(os).Accepted
	if *c.JudgmentRequests != 2 || *c.JudgmentAttempts != 0 || *c.JudgmentFallbacks != 2 || *c.FallbackFrequency != 1 || c.JudgmentUsage.CostUSD == nil || *c.JudgmentUsage.CostUSD != 0 || *c.JudgmentUsage.InputTokens != 0 || fake.Calls() != 1 {
		t.Fatalf("cooldown accounting=%+v", c)
	}
}

func TestRetriedFixtureAddsAttemptsOnly(t *testing.T) {
	var fixtures []llm.JudgmentResponse
	readFixture(t, "testdata/seed-judgments.json", &fixtures)
	failedUsage := usage(5, 0, .1)
	fake := llm.NewFakeJudgmentProvider(llm.JudgmentStep{Response: llm.JudgmentResponse{Usage: failedUsage}, Error: &llm.Error{Status: 503, Retryable: true}}, llm.JudgmentStep{Response: fixtures[0]})
	j, err := llm.NewJudgments(llm.JudgmentConfig{Enabled: true, Model: "fixture-judge-v1", Backoff: time.Millisecond}, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fixture-only" })
	if err != nil {
		t.Fatal(err)
	}
	r := j.Choice(t.Context(), "fixture", llm.JudgmentQuestion{ID: fixtures[0].Answers[0].ID, Question: "Recorded comparison", Options: []string{"changes", "restates", "abstain"}})
	if r.Fallback != "" || r.Attempts != 2 {
		t.Fatal(r)
	}
	o := observation("shadow")
	o.JudgmentAttempts = count(int64(r.Attempts))
	o.JudgmentUsage = r.Usage
	c := eval.SummarizePipelines([]l1.PipelineObservation{o}).Shadow
	if *c.JudgmentRequests != 1 || *c.JudgmentAttempts != 2 || *c.JudgmentUsage.InputTokens != 15 || *c.JudgmentUsage.CostUSD != .1 || *c.TotalCostUSD != .35 {
		t.Fatalf("retry accounting=%+v", c)
	}
}
