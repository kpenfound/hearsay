package llm_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
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
	cfg.Model = "pinned-v1"
	f := llm.NewFakeJudgmentProvider(steps...)
	j, err := llm.NewJudgments(cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return f }, func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	return j, f
}

func TestJevIsRequired(t *testing.T) {
	provider := func(llm.JudgmentConfig, string) llm.JudgmentProvider { return llm.NewFakeJudgmentProvider() }
	for _, tt := range []struct {
		name    string
		cfg     llm.JudgmentConfig
		key     string
		factory llm.JudgmentFactory
		want    string
	}{
		{"no model", llm.JudgmentConfig{}, "secret", provider, "set llm.judgments.model"},
		{"no key", llm.JudgmentConfig{Model: "pinned-v1"}, "", provider, "set HEARSAY_JEV_API_KEY"},
		{"no key in a named variable", llm.JudgmentConfig{Model: "pinned-v1", APIKeyEnv: "OPENROUTER_API_KEY"}, "", provider, "set OPENROUTER_API_KEY"},
		{"no provider", llm.JudgmentConfig{Model: "pinned-v1"}, "secret", nil, "no provider"},
		{"configured", llm.JudgmentConfig{Model: "pinned-v1"}, "secret", provider, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			j, err := llm.NewJudgments(tt.cfg, tt.factory, func(string) string { return tt.key })
			switch {
			case tt.want == "" && (err != nil || j == nil):
				t.Fatalf("NewJudgments() = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("NewJudgments() = %v, want an error naming %q", err, tt.want)
			}
		})
	}
}

func TestOnlyAConfidentYesCounts(t *testing.T) {
	yes, no := true, false
	cfg := llm.JudgmentConfig{Threshold: 0.8}
	for _, tt := range []struct {
		name string
		a    llm.JudgmentAnswer
		want bool
	}{
		{"a confident yes", llm.JudgmentAnswer{Yes: &yes, Confidence: 0.8}, true},
		{"an unconfident yes", llm.JudgmentAnswer{Yes: &yes, Confidence: 0.79}, false},
		{"a confident no", llm.JudgmentAnswer{Yes: &no, Confidence: 0.99}, false},
		{"no answer", llm.JudgmentAnswer{Confidence: 0.99}, false},
		{"a confidence out of range", llm.JudgmentAnswer{Yes: &yes, Confidence: math.NaN()}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.Yes(tt.a); got != tt.want {
				t.Fatalf("Yes() = %v", got)
			}
		})
	}
	if !(llm.JudgmentConfig{}).Yes(llm.JudgmentAnswer{Yes: &yes, Confidence: llm.DefaultJudgmentThreshold}) {
		t.Fatal("the default threshold does not admit a yes at the threshold")
	}
}

func TestAFailureIsAnError(t *testing.T) {
	j, _ := judgments(t, llm.JudgmentConfig{MaxRetries: -1}, llm.JudgmentStep{Error: &llm.Error{Status: 503, Msg: "private provider error"}})
	err := j.Judge(t.Context(), judgmentRequest()).Err()
	var je *llm.JudgmentError
	if !errors.As(err, &je) || je.Failure != llm.JudgmentUnavailable || strings.Contains(err.Error(), "private") {
		t.Fatalf("Err() = %v", err)
	}
	if st := j.Status(); st.Healthy || st.LastFailure != llm.JudgmentUnavailable || st.Failures != 1 {
		t.Fatalf("status=%+v", st)
	}
}

func TestJudgmentRetryAndBounds(t *testing.T) {
	transient := &llm.Error{Retryable: true, Status: 503, Msg: "private provider error"}
	for _, tt := range []struct {
		name     string
		cfg      llm.JudgmentConfig
		steps    []llm.JudgmentStep
		budget   time.Duration
		reason   llm.JudgmentFailure
		attempts int
	}{
		{"retry recovery", llm.JudgmentConfig{Backoff: time.Millisecond}, []llm.JudgmentStep{{Error: transient}, {Response: goodJudgment()}}, 0, "", 2},
		{"transport timeout recovery", llm.JudgmentConfig{Backoff: time.Millisecond}, []llm.JudgmentStep{{Error: context.DeadlineExceeded}, {Response: goodJudgment()}}, 0, "", 2},
		{"per attempt timeout", llm.JudgmentConfig{Timeout: 20 * time.Millisecond, TotalTimeout: time.Second, Backoff: time.Millisecond}, []llm.JudgmentStep{{Delay: time.Second}, {Response: goodJudgment()}}, 0, "", 2},
		{"bounded retries", llm.JudgmentConfig{MaxRetries: 2, Backoff: time.Millisecond}, []llm.JudgmentStep{{Error: transient}, {Error: transient}, {Error: transient}}, 0, llm.JudgmentUnavailable, 3},
		{"total timeout", llm.JudgmentConfig{Timeout: 10 * time.Millisecond, TotalTimeout: 25 * time.Millisecond, MaxRetries: 5, Backoff: time.Millisecond}, []llm.JudgmentStep{{Delay: time.Second}, {Delay: time.Second}, {Delay: time.Second}}, 0, llm.JudgmentTimeout, -1},
		{"caller cuts attempt", llm.JudgmentConfig{Timeout: time.Second}, []llm.JudgmentStep{{Delay: time.Second}}, 15 * time.Millisecond, llm.JudgmentTimeout, 1},
		{"caller cuts retries", llm.JudgmentConfig{Backoff: time.Second}, []llm.JudgmentStep{{Error: transient}}, 15 * time.Millisecond, llm.JudgmentTimeout, 1},
		{"caller cuts retry after", llm.JudgmentConfig{Backoff: time.Millisecond}, []llm.JudgmentStep{{Error: &llm.Error{Status: 429, RetryAfter: time.Hour}}}, 15 * time.Millisecond, llm.JudgmentTimeout, 1},
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
			if r.Failure != tt.reason || (tt.attempts >= 0 && r.Attempts != tt.attempts) {
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

func TestJudgmentRetryAfterIsHonored(t *testing.T) {
	for _, status := range []int{429, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			j, _ := judgments(t, llm.JudgmentConfig{Backoff: time.Millisecond}, llm.JudgmentStep{Error: &llm.Error{Status: status, RetryAfter: 15 * time.Millisecond}}, llm.JudgmentStep{Response: goodJudgment()})
			r := j.Judge(t.Context(), judgmentRequest())
			if r.Failure != "" || r.Attempts != 2 || r.Wait < 15*time.Millisecond {
				t.Fatalf("result=%+v", r)
			}
		})
	}
}

func TestJudgmentInvalidAnswerNeverRetries(t *testing.T) {
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
			j, f := judgments(t, llm.JudgmentConfig{}, llm.JudgmentStep{Response: bad, Error: tt.err}, llm.JudgmentStep{Response: bad, Error: tt.err}, llm.JudgmentStep{Response: goodJudgment()})
			for range 2 {
				r := j.Judge(t.Context(), judgmentRequest())
				if r.Failure != llm.JudgmentInvalidResponse || r.Attempts != 1 || r.Wait != 0 || len(r.Answers) != 0 {
					t.Fatalf("invalid result=%+v", r)
				}
				if tt.name == "negative input" && (r.Usage.InputTokens != nil || r.Usage.OutputTokens == nil || r.Usage.CostUSD == nil) {
					t.Fatalf("partial usage=%+v", r.Usage)
				}
			}
			r := j.Judge(t.Context(), judgmentRequest())
			if r.Failure != "" || f.Calls() != 3 {
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
			if r.Failure != "" || f.Requests()[0].State != "state" {
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
	if r := j.Judge(t.Context(), req); r.Failure != "" || f.Calls() != 1 || len(r.Answers) != 2 {
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
				want = llm.JudgmentTimeout
			}
			cancel()
			j, f := judgments(t, llm.JudgmentConfig{})
			r := j.Judge(ctx, judgmentRequest())
			if r.Failure != want || f.Calls() != 0 {
				t.Fatalf("result=%+v", r)
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
		{name: "invalid request", cfg: llm.JudgmentConfig{Model: "pinned-v1"}, credential: "fixture-only", attempts: 0},
		{name: "attempt without usage", cfg: llm.JudgmentConfig{Model: "pinned-v1", MaxRetries: -1}, credential: "fixture-only", steps: []llm.JudgmentStep{{Error: &llm.Error{Status: 503}}}, attempts: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := llm.NewFakeJudgmentProvider(tt.steps...)
			j, err := llm.NewJudgments(tt.cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return tt.credential })
			if err != nil {
				t.Fatal(err)
			}
			req := judgmentRequest()
			if tt.attempts == 0 {
				req.State = ""
			}
			r := j.Judge(t.Context(), req)
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

func TestUnconfiguredJevFailsEveryCall(t *testing.T) {
	r := llm.Unconfigured().Judge(t.Context(), judgmentRequest())
	if r.Err() == nil || r.Failure != llm.JudgmentUnavailable || len(r.Answers) != 0 {
		t.Fatalf("result=%+v", r)
	}
}

func TestFakeJudgmentsAnswerByRule(t *testing.T) {
	j := llm.FakeJudgments(func(state string, q llm.JudgmentQuestion) float64 {
		if q.ID == "a" {
			return 0.9
		}
		return 0.1
	})
	r := j.Judge(t.Context(), llm.JudgmentRequest{State: "s", Questions: []llm.JudgmentQuestion{{ID: "a", Kind: llm.JudgmentYesNo, Question: "a?"}, {ID: "b", Kind: llm.JudgmentYesNo, Question: "b?"}}})
	if r.Err() != nil || !j.Config().Yes(r.Answers[0]) || j.Config().Yes(r.Answers[1]) {
		t.Fatalf("result=%+v", r)
	}
}
