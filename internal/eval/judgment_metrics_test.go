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
func observation(stage string) l1.PipelineObservation {
	return l1.PipelineObservation{Stage: stage, Cohort: "accepted", HandlerNS: count(int64(time.Second)), QueueDelayNS: count(int64(2 * time.Second)), GenerativeCalls: count(2), GenerativeAttempts: count(3), GenerativeUsage: usage(100, 10, .25), JudgmentRequests: count(1), JudgmentAttempts: count(1), JudgmentUsage: usage(20, 1, .05), CandidateCount: count(5)}
}

func pipelineReport() eval.PipelineReport {
	a, b, c := observation("distill"), observation("assert"), observation("assert")
	c.JudgmentUsage = llm.JudgmentUsage{}
	b.JudgmentAttempts = count(3)
	return eval.SummarizePipelines([]l1.PipelineObservation{a, b, c, observation("replay")})
}

func TestPipelineDistillTotals(t *testing.T) {
	m := pipelineReport().Distill
	if m.Runs != 1 || m.TotalCostUSD == nil || math.Abs(*m.TotalCostUSD-.30) > 1e-12 || *m.GenerativeCalls != 2 || m.GenerativeAttempts == nil || *m.GenerativeAttempts != 3 || *m.CandidateCount != 5 || *m.HandlerSeconds != 1 || *m.QueueDelaySeconds != 2 {
		t.Fatalf("distill=%+v", m)
	}
}

func TestPipelineAssertTotalsLeaveCostUnknownWhenAnyRunHasNone(t *testing.T) {
	m := pipelineReport().Assert
	if m.Runs != 2 || *m.JudgmentRequests != 2 || *m.JudgmentAttempts != 4 || m.TotalCostUSD != nil {
		t.Fatalf("assert=%+v", m)
	}
}

func TestPipelineCountsRunsOfAnotherStageAsUnclassified(t *testing.T) {
	if got := pipelineReport().UnclassifiedRuns; got != 1 {
		t.Fatalf("UnclassifiedRuns = %d, want 1", got)
	}
}

func TestPipelineUnknownAndOverflow(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*l1.PipelineObservation)
		check  func(*testing.T, eval.PipelineStage)
	}{
		{"unknown usage", func(o *l1.PipelineObservation) { o.JudgmentUsage.InputTokens = nil; o.JudgmentUsage.CostUSD = nil }, func(t *testing.T, c eval.PipelineStage) {
			if c.JudgmentUsage.InputTokens != nil || c.TotalCostUSD != nil || c.JudgmentUsage.OutputTokens == nil {
				t.Fatal(c)
			}
		}},
		{"token overflow", func(o *l1.PipelineObservation) {
			o.GenerativeUsage.InputTokens = count(math.MaxInt64)
			o.JudgmentUsage.OutputTokens = count(math.MaxInt64)
		}, func(t *testing.T, c eval.PipelineStage) {
			if c.GenerativeUsage.InputTokens != nil || c.JudgmentUsage.OutputTokens != nil {
				t.Fatal(c)
			}
		}},
		{"cost overflow", func(o *l1.PipelineObservation) {
			o.GenerativeUsage.CostUSD = share(math.MaxFloat64)
			o.JudgmentUsage.CostUSD = share(math.MaxFloat64)
		}, func(t *testing.T, c eval.PipelineStage) {
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
		}, func(t *testing.T, c eval.PipelineStage) {
			if c.GenerativeCalls != nil || c.GenerativeAttempts != nil || c.HandlerSeconds != nil || c.MeanHandlerSeconds != nil || c.JudgmentAttempts != nil || c.CandidateCount != nil {
				t.Fatal(c)
			}
		}},
		{"unknown delay", func(o *l1.PipelineObservation) { o.QueueDelayNS = nil }, func(t *testing.T, c eval.PipelineStage) {
			if c.QueueDelaySeconds != nil || c.MeanQueueDelaySeconds == nil || *c.MeanQueueDelaySeconds != 2 || c.MissingQueueDelayRuns != 2 {
				t.Fatal(c)
			}
		}},
		{"nonfinite", func(o *l1.PipelineObservation) { o.JudgmentUsage.CostUSD = share(math.Inf(1)) }, func(t *testing.T, c eval.PipelineStage) {
			if c.TotalCostUSD != nil || c.JudgmentUsage.CostUSD != nil {
				t.Fatal(c)
			}
		}},
		{"negative", func(o *l1.PipelineObservation) {
			o.JudgmentUsage.InputTokens = count(-1)
			o.JudgmentUsage.CostUSD = share(-1)
		}, func(t *testing.T, c eval.PipelineStage) {
			if c.JudgmentUsage.InputTokens != nil || c.TotalCostUSD != nil {
				t.Fatal(c)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := observation("assert")
			tt.mutate(&o)
			report := eval.SummarizePipelines([]l1.PipelineObservation{o, o, observation("assert")})
			tt.check(t, report.Assert)
			if _, err := json.Marshal(report); err != nil {
				t.Fatalf("report is not valid JSON: %v", err)
			}
		})
	}
}

func TestPipelineCostOverflowOfOneRunIsUnknown(t *testing.T) {
	o := observation("assert")
	o.GenerativeUsage.CostUSD = share(math.MaxFloat64)
	o.JudgmentUsage.CostUSD = share(math.MaxFloat64)
	if c := eval.SummarizePipelines([]l1.PipelineObservation{o}).Assert; c.TotalCostUSD != nil {
		t.Fatal(c)
	}
}

func TestPipelineWithNoObservationsReportsNothing(t *testing.T) {
	r := eval.SummarizePipelines(nil)
	for _, c := range []eval.PipelineStage{r.Distill, r.Assert} {
		if c.Runs != 0 || c.TotalCostUSD != nil || c.GenerativeCalls != nil || c.HandlerSeconds != nil || c.QueueDelaySeconds != nil {
			t.Fatal(c)
		}
	}
}

func TestPipelinePartialQueueDelayLeavesTheTotalUnknownAndTheMeanKnown(t *testing.T) {
	a, b := observation("distill"), observation("distill")
	b.QueueDelayNS = nil
	c := eval.SummarizePipelines([]l1.PipelineObservation{a, b}).Distill
	if c.QueueDelaySeconds != nil || c.MeanQueueDelaySeconds == nil || *c.MeanQueueDelaySeconds != 2 || c.MissingQueueDelayRuns != 1 {
		t.Fatal(c)
	}
}

func TestRetriedFixtureAddsAttemptsOnly(t *testing.T) {
	var fixtures []llm.JudgmentResponse
	readFixture(t, "testdata/seed-judgments.json", &fixtures)
	failedUsage := usage(5, 0, .1)
	fake := llm.NewFakeJudgmentProvider(llm.JudgmentStep{Response: llm.JudgmentResponse{Usage: failedUsage}, Error: &llm.Error{Status: 503, Retryable: true}}, llm.JudgmentStep{Response: fixtures[0]})
	j, err := llm.NewJudgments(llm.JudgmentConfig{Model: "fixture-judge-v1", Backoff: time.Millisecond}, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fixture-only" })
	if err != nil {
		t.Fatal(err)
	}
	r := j.YesNo(t.Context(), "fixture", llm.JudgmentQuestion{ID: fixtures[0].Answers[0].ID, Question: "Recorded comparison"})
	if r.Failure != "" || r.Attempts != 2 {
		t.Fatal(r)
	}
	o := observation("assert")
	o.JudgmentAttempts = count(int64(r.Attempts))
	o.JudgmentUsage = r.Usage
	c := eval.SummarizePipelines([]l1.PipelineObservation{o}).Assert
	if *c.JudgmentRequests != 1 || *c.JudgmentAttempts != 2 || *c.JudgmentUsage.InputTokens != 15 || *c.JudgmentUsage.CostUSD != .1 || *c.TotalCostUSD != .35 {
		t.Fatalf("retry accounting=%+v", c)
	}
}
