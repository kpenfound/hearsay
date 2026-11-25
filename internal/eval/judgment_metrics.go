package eval

import (
	"math"
	"slices"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
)

// PipelineStage is one stage's retained work (distill or assert), not
// documents or replay runs. All consumption totals become null when any
// contributor is unknown or invalid.
type PipelineStage struct {
	Runs                  int               `json:"runs"`
	HandlerSeconds        *float64          `json:"handler_seconds"`
	MeanHandlerSeconds    *float64          `json:"mean_handler_seconds"`
	P90HandlerSeconds     *float64          `json:"p90_handler_seconds"`
	MissingHandlerRuns    int               `json:"missing_handler_runs"`
	QueueDelaySeconds     *float64          `json:"queue_delay_seconds"`
	MeanQueueDelaySeconds *float64          `json:"mean_queue_delay_seconds"`
	MissingQueueDelayRuns int               `json:"missing_queue_delay_runs"`
	GenerativeCalls       *int64            `json:"generative_calls"`
	GenerativeAttempts    *int64            `json:"generative_attempts"`
	GenerativeUsage       llm.JudgmentUsage `json:"generative_usage"`
	JudgmentRequests      *int64            `json:"judgment_requests"`
	JudgmentAttempts      *int64            `json:"judgment_attempts"`
	JudgmentUsage         llm.JudgmentUsage `json:"judgment_usage"`
	CandidateCount        *int64            `json:"candidate_count"`
	TotalCostUSD          *float64          `json:"total_cost_usd"`
}

// PipelineReport is the recorded work of each stage: model calls, Jev
// requests, their consumption and time.
type PipelineReport struct {
	Distill          PipelineStage `json:"distill"`
	Assert           PipelineStage `json:"assert"`
	UnclassifiedRuns int           `json:"unclassified_runs"`
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

func pipelineStage(os []l1.PipelineObservation) PipelineStage {
	c := PipelineStage{Runs: len(os)}
	if len(os) == 0 {
		return c
	}
	zero := int64(0)
	costZero := float64(0)
	c.GenerativeCalls = &zero
	c.GenerativeAttempts = &zero
	c.JudgmentRequests = &zero
	c.JudgmentAttempts = &zero
	c.CandidateCount = &zero
	c.GenerativeUsage = llm.JudgmentUsage{InputTokens: &zero, OutputTokens: &zero, CostUSD: &costZero}
	c.JudgmentUsage = c.GenerativeUsage
	handler, queue := &zero, &zero
	var durations []int64
	knownQueue := &zero
	knownQueues := 0
	for _, o := range os {
		c.GenerativeCalls = l1.AddPipelineCount(c.GenerativeCalls, o.GenerativeCalls)
		c.GenerativeAttempts = l1.AddPipelineCount(c.GenerativeAttempts, o.GenerativeAttempts)
		c.JudgmentRequests = l1.AddPipelineCount(c.JudgmentRequests, o.JudgmentRequests)
		c.JudgmentAttempts = l1.AddPipelineCount(c.JudgmentAttempts, o.JudgmentAttempts)
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
	return c
}

// SummarizePipelines totals each stage's observations, retries, generation and
// candidate processing included as recorded.
func SummarizePipelines(os []l1.PipelineObservation) PipelineReport {
	stages := map[string][]l1.PipelineObservation{}
	unknown := 0
	for _, o := range os {
		switch o.Stage {
		case "distill", "assert":
			stages[o.Stage] = append(stages[o.Stage], o)
		default:
			unknown++
		}
	}
	return PipelineReport{Distill: pipelineStage(stages["distill"]), Assert: pipelineStage(stages["assert"]), UnclassifiedRuns: unknown}
}
