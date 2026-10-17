package llm_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
)

func judgmentRequest() llm.JudgmentRequest {
	return llm.JudgmentRequest{State: "private source text", Questions: []llm.JudgmentQuestion{{ID: "q", Kind: llm.JudgmentChoice, Question: "private question", Options: []string{"change", "restate", "abstain"}}}}
}
func goodJudgment() llm.JudgmentResponse {
	zero := int64(0)
	cost := 0.0
	return llm.JudgmentResponse{ModelVersion: "pinned-v1", Answers: []llm.JudgmentAnswer{{ID: "q", Choice: "change", Confidence: 0.8, Probabilities: map[string]float64{"change": 0.8, "restate": 0.2, "abstain": 0}}}, Usage: llm.JudgmentUsage{InputTokens: &zero, OutputTokens: &zero, CostUSD: &cost}}
}
func judgments(t *testing.T, cfg llm.JudgmentConfig, steps ...llm.JudgmentStep) (llm.Judgments, *llm.FakeJudgmentProvider) {
	t.Helper()
	cfg.Enabled = true
	cfg.Model = "pinned-v1"
	f := llm.NewFakeJudgmentProvider(steps...)
	j, err := llm.NewJudgments(cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return f }, func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	return j, f
}

func TestJudgmentsDisabledAndMissingCredential(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cfg    llm.JudgmentConfig
		reason llm.JudgmentFallback
		reads  int
	}{
		{"absent", llm.JudgmentConfig{}, llm.JudgmentDisabled, 0},
		{"off", llm.JudgmentConfig{Model: "pinned-v1"}, llm.JudgmentDisabled, 0},
		{"missing credential", llm.JudgmentConfig{Enabled: true, Model: "pinned-v1"}, llm.JudgmentMissingCredential, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := llm.NewFakeJudgmentProvider()
			reads, built := 0, 0
			j, err := llm.NewJudgments(tt.cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { built++; return f }, func(string) string { reads++; return "" })
			if err != nil {
				t.Fatal(err)
			}
			r := j.Judge(t.Context(), judgmentRequest())
			if reads != tt.reads || built != 0 || f.Calls() != 0 || r.Attempts != 0 || r.Fallback != tt.reason {
				t.Fatalf("reads=%d built=%d calls=%d result=%+v", reads, built, f.Calls(), r)
			}
			if j.Status().Observations != 1 || j.Status().Fallbacks != 1 {
				t.Fatalf("status=%+v", j.Status())
			}
		})
	}
}

func TestJudgmentRetryAndBounds(t *testing.T) {
	transient := &llm.Error{Retryable: true, Status: 503, Msg: "private provider error"}
	for _, tt := range []struct {
		name     string
		cfg      llm.JudgmentConfig
		steps    []llm.JudgmentStep
		budget   time.Duration
		reason   llm.JudgmentFallback
		attempts int
	}{
		{"retry recovery", llm.JudgmentConfig{Backoff: time.Millisecond}, []llm.JudgmentStep{{Error: transient}, {Response: goodJudgment()}}, 0, "", 2},
		{"transport timeout recovery", llm.JudgmentConfig{Backoff: time.Millisecond}, []llm.JudgmentStep{{Error: context.DeadlineExceeded}, {Response: goodJudgment()}}, 0, "", 2},
		{"per attempt timeout", llm.JudgmentConfig{Timeout: 20 * time.Millisecond, TotalTimeout: time.Second, Backoff: time.Millisecond}, []llm.JudgmentStep{{Delay: time.Second}, {Response: goodJudgment()}}, 0, "", 2},
		{"bounded retries", llm.JudgmentConfig{MaxRetries: 2, Backoff: time.Millisecond}, []llm.JudgmentStep{{Error: transient}, {Error: transient}, {Error: transient}}, 0, llm.JudgmentUnavailable, 3},
		{"total timeout", llm.JudgmentConfig{Timeout: 10 * time.Millisecond, TotalTimeout: 25 * time.Millisecond, MaxRetries: 5, Backoff: time.Millisecond}, []llm.JudgmentStep{{Delay: time.Second}, {Delay: time.Second}, {Delay: time.Second}}, 0, llm.JudgmentTimeout, -1},
		{"caller cuts attempt", llm.JudgmentConfig{Timeout: time.Second}, []llm.JudgmentStep{{Delay: time.Second}}, 15 * time.Millisecond, llm.JudgmentBudgetExhausted, 1},
		{"caller cuts retries", llm.JudgmentConfig{Backoff: time.Second}, []llm.JudgmentStep{{Error: transient}}, 15 * time.Millisecond, llm.JudgmentBudgetExhausted, 1},
		{"caller cuts retry after", llm.JudgmentConfig{Backoff: time.Millisecond}, []llm.JudgmentStep{{Error: &llm.Error{Status: 429, RetryAfter: time.Hour}}}, 15 * time.Millisecond, llm.JudgmentBudgetExhausted, 1},
		{"total cuts retry after", llm.JudgmentConfig{TotalTimeout: 15 * time.Millisecond}, []llm.JudgmentStep{{Error: &llm.Error{Status: 529, RetryAfter: time.Hour}}}, 0, llm.JudgmentTimeout, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			j, _ := judgments(t, tt.cfg, tt.steps...)
			ctx := t.Context()
			if tt.budget > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.budget)
				defer cancel()
			}
			start := time.Now()
			r := j.Judge(ctx, judgmentRequest())
			if time.Since(start) > 2*time.Second {
				t.Fatalf("call exceeded bound: %v", time.Since(start))
			}
			if r.Fallback != tt.reason || (tt.attempts >= 0 && r.Attempts != tt.attempts) {
				t.Fatalf("result=%+v want reason=%s attempts=%d", r, tt.reason, tt.attempts)
			}
			if tt.reason == "" && !j.Status().Healthy {
				t.Fatalf("recovery status=%+v", j.Status())
			}
			if tt.name == "retry recovery" && r.Usage.InputTokens != nil {
				t.Fatal("unknown failed attempt must propagate unknown accounting")
			}
		})
	}
}

func TestJudgmentRateLimitsAndCooldownAcrossCallers(t *testing.T) {
	for _, status := range []int{429, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			const cooldown = 250 * time.Millisecond
			j, f := judgments(t, llm.JudgmentConfig{CooldownFailures: 1, Cooldown: cooldown, MaxRetries: -1}, llm.JudgmentStep{Error: &llm.Error{Status: status, RetryAfter: time.Second}}, llm.JudgmentStep{Response: goodJudgment()})
			first := j.Judge(t.Context(), judgmentRequest())
			if first.Fallback != llm.JudgmentRateLimited {
				t.Fatal(first)
			}
			done := make(chan llm.JudgmentResult, 2)
			for range 2 {
				go func() { done <- j.Judge(t.Context(), judgmentRequest()) }()
			}
			for range 2 {
				r := <-done
				if r.Usage.CostUSD == nil || *r.Usage.CostUSD != 0 || r.Usage.InputTokens == nil || *r.Usage.InputTokens != 0 || r.Usage.OutputTokens == nil || *r.Usage.OutputTokens != 0 {
					t.Fatalf("suppressed usage must be known zero: %+v", r.Usage)
				}
				if r.Fallback != llm.JudgmentCooldown || r.Attempts != 0 {
					t.Fatal(r)
				}
			}
			if f.Calls() != 1 || j.Status().Observations != 3 || j.Status().Attempts != 1 {
				t.Fatalf("calls=%d status=%+v", f.Calls(), j.Status())
			}
			time.Sleep(cooldown)
			r := j.Judge(t.Context(), judgmentRequest())
			if r.Fallback != "" || !j.Status().Healthy {
				t.Fatalf("recovery=%+v status=%+v", r, j.Status())
			}
		})
	}
}

func TestJudgmentRetryAfterIsHonored(t *testing.T) {
	for _, status := range []int{429, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			j, _ := judgments(t, llm.JudgmentConfig{Backoff: time.Millisecond}, llm.JudgmentStep{Error: &llm.Error{Status: status, RetryAfter: 15 * time.Millisecond}}, llm.JudgmentStep{Response: goodJudgment()})
			r := j.Judge(t.Context(), judgmentRequest())
			if r.Fallback != "" || r.Attempts != 2 || r.Wait < 15*time.Millisecond {
				t.Fatalf("result=%+v", r)
			}
		})
	}
}

func TestJudgmentInvalidAnswerNeverRetriesOrCools(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*llm.JudgmentResponse)
		err    error
	}{
		{"wrapped marker", func(*llm.JudgmentResponse) {}, &llm.Error{Retryable: true, Err: fmt.Errorf("wrapped: %w", llm.ErrJudgmentResponse), Msg: "secret answer"}},
		{"missing probability", func(r *llm.JudgmentResponse) { delete(r.Answers[0].Probabilities, "abstain") }, nil},
		{"non normalized", func(r *llm.JudgmentResponse) { r.Answers[0].Probabilities["change"] = 0.9 }, nil},
		{"non finite", func(r *llm.JudgmentResponse) { r.Answers[0].Confidence = math.NaN() }, nil},
		{"wrong answer type", func(r *llm.JudgmentResponse) { b := true; r.Answers[0].Yes = &b }, nil},
		{"missing version", func(r *llm.JudgmentResponse) { r.ModelVersion = "" }, nil},
		{"missing answer", func(r *llm.JudgmentResponse) { r.Answers = nil }, nil},
		{"negative input", func(r *llm.JudgmentResponse) { n := int64(-1); r.Usage.InputTokens = &n }, nil},
		{"negative output", func(r *llm.JudgmentResponse) { n := int64(-1); r.Usage.OutputTokens = &n }, nil},
		{"negative cost", func(r *llm.JudgmentResponse) { n := -1.0; r.Usage.CostUSD = &n }, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bad := goodJudgment()
			tt.mutate(&bad)
			j, f := judgments(t, llm.JudgmentConfig{CooldownFailures: 1}, llm.JudgmentStep{Response: bad, Error: tt.err}, llm.JudgmentStep{Response: bad, Error: tt.err}, llm.JudgmentStep{Response: goodJudgment()})
			for range 2 {
				r := j.Judge(t.Context(), judgmentRequest())
				if r.Fallback != llm.JudgmentInvalidResponse || r.Attempts != 1 || r.Wait != 0 || len(r.Answers) != 0 {
					t.Fatalf("invalid result=%+v", r)
				}
				if tt.name == "negative input" && (r.Usage.InputTokens != nil || r.Usage.OutputTokens == nil || r.Usage.CostUSD == nil) {
					t.Fatalf("partial usage=%+v", r.Usage)
				}
			}
			r := j.Judge(t.Context(), judgmentRequest())
			if r.Fallback != "" || f.Calls() != 3 {
				t.Fatalf("recovery=%+v calls=%d", r, f.Calls())
			}
		})
	}
}

func TestJudgmentAccounting(t *testing.T) {
	zero := int64(0)
	max := int64(math.MaxInt64)
	one := int64(1)
	for _, tt := range []struct {
		name  string
		a, b  llm.JudgmentUsage
		known bool
		want  int64
	}{
		{"reported zero", llm.JudgmentUsage{InputTokens: &zero}, llm.JudgmentUsage{InputTokens: &zero}, true, 0},
		{"missing", llm.JudgmentUsage{}, llm.JudgmentUsage{InputTokens: &zero}, false, 0},
		{"overflow", llm.JudgmentUsage{InputTokens: &max}, llm.JudgmentUsage{InputTokens: &one}, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := llm.AddJudgmentUsage(tt.a, tt.b)
			if (r.InputTokens != nil) != tt.known || r.InputTokens != nil && *r.InputTokens != tt.want {
				t.Fatalf("usage=%+v", r)
			}
		})
	}
}

func TestJudgmentPrimitivesAndSharedState(t *testing.T) {
	n := 2.25
	b := true
	q := judgmentRequest().Questions[0]
	for _, tt := range []struct {
		name string
		q    llm.JudgmentQuestion
		a    llm.JudgmentAnswer
	}{
		{"choice", q, goodJudgment().Answers[0]},
		{"score", llm.JudgmentQuestion{ID: "q", Question: "score", Min: 1, Max: 3, Rubric: "rubric"}, llm.JudgmentAnswer{ID: "q", Score: &n, Confidence: 0.7, Probabilities: map[string]float64{"1": 0.1, "2": 0.7, "3": 0.2}}},
		{"yes no", llm.JudgmentQuestion{ID: "q", Question: "yes?"}, llm.JudgmentAnswer{ID: "q", Yes: &b, Confidence: 0.9, Probabilities: map[string]float64{"yes": 0.9, "no": 0.1}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			j, f := judgments(t, llm.JudgmentConfig{}, llm.JudgmentStep{Response: llm.JudgmentResponse{ModelVersion: "v1", Answers: []llm.JudgmentAnswer{tt.a}}})
			var r llm.JudgmentResult
			switch tt.name {
			case "choice":
				r = j.Choice(t.Context(), "state", tt.q)
			case "score":
				r = j.Score(t.Context(), "state", tt.q)
			case "yes no":
				r = j.YesNo(t.Context(), "state", tt.q)
			}
			if r.Fallback != "" || f.Requests()[0].State != "state" {
				t.Fatalf("result=%+v", r)
			}
		})
	}
	req := judgmentRequest()
	second := req.Questions[0]
	second.ID = "other"
	req.Questions = append(req.Questions, second)
	response := goodJudgment()
	a := response.Answers[0]
	a.ID = "other"
	response.Answers = append(response.Answers, a)
	j, f := judgments(t, llm.JudgmentConfig{}, llm.JudgmentStep{Response: response})
	if r := j.Judge(t.Context(), req); r.Fallback != "" || f.Calls() != 1 || len(r.Answers) != 2 {
		t.Fatalf("batch=%+v", r)
	}
}

func TestJudgmentCallerAlreadyDone(t *testing.T) {
	for _, expired := range []bool{true, false} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			want := llm.JudgmentCanceled
			if expired {
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = llm.JudgmentBudgetExhausted
			}
			cancel()
			j, f := judgments(t, llm.JudgmentConfig{})
			r := j.Judge(ctx, judgmentRequest())
			if r.Fallback != want || f.Calls() != 0 {
				t.Fatalf("result=%+v", r)
			}
		})
	}
	if !errors.Is(fmt.Errorf("outer: %w", llm.ErrJudgmentResponse), llm.ErrJudgmentResponse) {
		t.Fatal("marker lost")
	}
}

type judgmentProviderFunc func(context.Context, llm.JudgmentRequest) (llm.JudgmentResponse, error)

func (f judgmentProviderFunc) Judge(ctx context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	return f(ctx, req)
}

func TestJudgmentInflightAnswerCannotShortenCooldown(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			response := goodJudgment()
			if invalid {
				response.Answers = nil
			}
			f := llm.NewFakeJudgmentProvider(
				llm.JudgmentStep{Response: response},
				llm.JudgmentStep{Error: &llm.Error{Status: 429}},
			)
			started, release := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			var calls atomic.Int64
			provider := judgmentProviderFunc(func(ctx context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
				n := calls.Add(1)
				r, err := f.Judge(ctx, req)
				if n == 1 {
					// Hold the first response until another caller opens the cooldown.
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return llm.JudgmentResponse{}, ctx.Err()
					}
				}
				return r, err
			})
			j, err := llm.NewJudgments(llm.JudgmentConfig{Enabled: true, Model: "pinned-v1", CooldownFailures: 1, Cooldown: time.Second, MaxRetries: -1},
				func(llm.JudgmentConfig, string) llm.JudgmentProvider { return provider }, func(string) string { return "secret" })
			if err != nil {
				t.Fatal(err)
			}
			first := make(chan llm.JudgmentResult, 1)
			go func() { first <- j.Judge(t.Context(), judgmentRequest()) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("first caller did not enter provider")
			}
			if r := j.Judge(t.Context(), judgmentRequest()); r.Fallback != llm.JudgmentRateLimited {
				t.Fatal(r)
			}
			release <- struct{}{}
			<-first
			r := j.Judge(t.Context(), judgmentRequest())
			if r.Fallback != llm.JudgmentCooldown || r.Attempts != 0 || f.Calls() != 2 {
				t.Fatalf("result=%+v calls=%d", r, f.Calls())
			}
		})
	}
}

func TestJudgmentZeroAttemptsVersusUnknownAttemptUsage(t *testing.T) {
	for _, tt := range []struct {
		name       string
		cfg        llm.JudgmentConfig
		credential string
		steps      []llm.JudgmentStep
		attempts   int
	}{
		{name: "disabled", cfg: llm.JudgmentConfig{}, attempts: 0},
		{name: "missing credential", cfg: llm.JudgmentConfig{Enabled: true, Model: "pinned-v1"}, attempts: 0},
		{name: "attempt without usage", cfg: llm.JudgmentConfig{Enabled: true, Model: "pinned-v1", MaxRetries: -1}, credential: "fixture-only", steps: []llm.JudgmentStep{{Error: &llm.Error{Status: 503}}}, attempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := llm.NewFakeJudgmentProvider(tt.steps...)
			j, err := llm.NewJudgments(tt.cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return tt.credential })
			if err != nil {
				t.Fatal(err)
			}
			r := j.Judge(t.Context(), judgmentRequest())
			if r.Attempts != tt.attempts {
				t.Fatal(r)
			}
			if tt.attempts == 0 {
				if r.Usage.InputTokens == nil || *r.Usage.InputTokens != 0 || r.Usage.OutputTokens == nil || *r.Usage.OutputTokens != 0 || r.Usage.CostUSD == nil || *r.Usage.CostUSD != 0 {
					t.Fatalf("zero-attempt consumption=%+v", r.Usage)
				}
			} else if r.Usage.InputTokens != nil || r.Usage.OutputTokens != nil || r.Usage.CostUSD != nil {
				t.Fatalf("unknown attempt consumption=%+v", r.Usage)
			}
		})
	}
}
