package eval

import (
	"math"
	"slices"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
)

// PipelineCohort is retained worker-stage work, not documents or replay runs.
// All consumption totals become null when any contributor is unknown/invalid.
type PipelineCohort struct {
	Runs                  int               `json:"runs"`
	HandlerSeconds        *float64          `json:"handler_seconds"`
	MeanHandlerSeconds    *float64          `json:"mean_handler_seconds"`
	P90HandlerSeconds     *float64          `json:"p90_handler_seconds"`
	MissingHandlerRuns    int               `json:"missing_handler_runs"`
	QueueDelaySeconds     *float64          `json:"queue_delay_seconds"`
	MeanQueueDelaySeconds *float64          `json:"mean_queue_delay_seconds"`
	MissingQueueDelayRuns int               `json:"missing_queue_delay_runs"`
	GenerativeCalls       *int64            `json:"generative_calls"`
	GenerativeUsage       llm.JudgmentUsage `json:"generative_usage"`
	JudgmentRequests      *int64            `json:"judgment_requests"`
	JudgmentAttempts      *int64            `json:"judgment_attempts"`
	JudgmentFallbacks     *int64            `json:"judgment_fallbacks"`
	FallbackFrequency     *float64          `json:"fallback_frequency"`
	JudgmentUsage         llm.JudgmentUsage `json:"judgment_usage"`
	CandidateCount        *int64            `json:"candidate_count"`
	TotalCostUSD          *float64          `json:"total_cost_usd"`
}

// PipelineReport deliberately contains no savings estimate. Cohorts need
// comparable workloads and complete accounting before any causal comparison.
type PipelineReport struct {
	Baseline         PipelineCohort `json:"baseline"`
	Shadow           PipelineCohort `json:"shadow"`
	Accepted         PipelineCohort `json:"accepted"`
	UnclassifiedRuns int            `json:"unclassified_runs"`
}

func addCost(a, b *float64) *float64 {
	if a == nil || b == nil || *a < 0 || *b < 0 || math.IsNaN(*a) || math.IsNaN(*b) || math.IsInf(*a, 0) || math.IsInf(*b, 0) || (*a > math.MaxFloat64-*b || *b > math.MaxFloat64-*a) {
		return nil
	}
	n := *a + *b
	if math.IsInf(n, 0) {
		return nil
	}
	return &n
}

func addUsage(a, b llm.JudgmentUsage) llm.JudgmentUsage {
	return llm.JudgmentUsage{InputTokens: l1.AddPipelineCount(a.InputTokens, b.InputTokens), OutputTokens: l1.AddPipelineCount(a.OutputTokens, b.OutputTokens), CostUSD: addCost(a.CostUSD, b.CostUSD)}
}

func nsSeconds(n *int64) *float64 {
	if n == nil || *n < 0 {
		return nil
	}
	s := float64(*n) / 1e9
	return &s
}

func pipelineCohort(os []l1.PipelineObservation) PipelineCohort {
	c := PipelineCohort{Runs: len(os)}
	if len(os) == 0 {
		return c
	}
	zero := int64(0)
	costZero := float64(0)
	c.GenerativeCalls = &zero
	c.JudgmentRequests = &zero
	c.JudgmentAttempts = &zero
	c.JudgmentFallbacks = &zero
	c.CandidateCount = &zero
	c.GenerativeUsage = llm.JudgmentUsage{InputTokens: &zero, OutputTokens: &zero, CostUSD: &costZero}
	c.JudgmentUsage = c.GenerativeUsage
	handler, queue := &zero, &zero
	var durations []int64
	// A separate known-only queue sum supplies its mean; the total remains null
	// if some delays are missing. Integer overflow still makes the mean unknown.
	knownQueue := &zero
	knownQueues := 0
	for _, o := range os {
		c.GenerativeCalls = l1.AddPipelineCount(c.GenerativeCalls, o.GenerativeCalls)
		c.JudgmentRequests = l1.AddPipelineCount(c.JudgmentRequests, o.JudgmentRequests)
		c.JudgmentAttempts = l1.AddPipelineCount(c.JudgmentAttempts, o.JudgmentAttempts)
		c.JudgmentFallbacks = l1.AddPipelineCount(c.JudgmentFallbacks, o.JudgmentFallbacks)
		c.CandidateCount = l1.AddPipelineCount(c.CandidateCount, o.CandidateCount)
		c.GenerativeUsage = addUsage(c.GenerativeUsage, o.GenerativeUsage)
		c.JudgmentUsage = addUsage(c.JudgmentUsage, o.JudgmentUsage)
		handler = l1.AddPipelineCount(handler, o.HandlerNS)
		queue = l1.AddPipelineCount(queue, o.QueueDelayNS)
		if o.HandlerNS == nil || *o.HandlerNS < 0 {
			c.MissingHandlerRuns++
		} else {
			durations = append(durations, *o.HandlerNS)
		}
		if o.QueueDelayNS == nil || *o.QueueDelayNS < 0 {
			c.MissingQueueDelayRuns++
		} else {
			knownQueue = l1.AddPipelineCount(knownQueue, o.QueueDelayNS)
			knownQueues++
		}
	}
	c.TotalCostUSD = addCost(c.GenerativeUsage.CostUSD, c.JudgmentUsage.CostUSD)
	c.HandlerSeconds = nsSeconds(handler)
	c.QueueDelaySeconds = nsSeconds(queue)
	if c.HandlerSeconds != nil {
		n := *c.HandlerSeconds / float64(c.Runs)
		c.MeanHandlerSeconds = &n
	}
	if c.MissingHandlerRuns == 0 {
		slices.Sort(durations)
		i := max(0, int(math.Ceil(.9*float64(len(durations))))-1)
		c.P90HandlerSeconds = nsSeconds(&durations[i])
	}
	if knownQueues > 0 && knownQueue != nil {
		n := float64(*knownQueue) / 1e9 / float64(knownQueues)
		c.MeanQueueDelaySeconds = &n
	}
	if c.JudgmentRequests != nil && c.JudgmentFallbacks != nil && *c.JudgmentRequests > 0 && *c.JudgmentFallbacks <= *c.JudgmentRequests {
		n := float64(*c.JudgmentFallbacks) / float64(*c.JudgmentRequests)
		c.FallbackFrequency = &n
	}
	return c
}

// SummarizePipelines includes retries, generation and candidate processing as
// recorded; no failed/fallback run is moved out of its intended cohort.
func SummarizePipelines(os []l1.PipelineObservation) PipelineReport {
	cohorts := map[string][]l1.PipelineObservation{}
	unknown := 0
	for _, o := range os {
		switch o.Cohort {
		case "baseline", "shadow", "accepted":
			cohorts[o.Cohort] = append(cohorts[o.Cohort], o)
		default:
			unknown++
		}
	}
	return PipelineReport{Baseline: pipelineCohort(cohorts["baseline"]), Shadow: pipelineCohort(cohorts["shadow"]), Accepted: pipelineCohort(cohorts["accepted"]), UnclassifiedRuns: unknown}
}
