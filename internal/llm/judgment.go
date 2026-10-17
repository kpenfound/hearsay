package llm

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"
)

// JudgmentKind selects a typed primitive, independently of the generative tiers.
type JudgmentKind string

// The supported primitives. Score rates an ordered rubric of integer levels.
const (
	JudgmentChoice JudgmentKind = "choice"
	JudgmentScore  JudgmentKind = "score"
	JudgmentYesNo  JudgmentKind = "yes_no"
)

// JudgmentQuestion is one independent question over a request's shared state.
// IDs and option labels are caller-owned; prompts belong to the caller.
type JudgmentQuestion struct {
	ID       string
	Kind     JudgmentKind
	Question string
	Options  []string
	Rubric   string
	Min, Max int
}

// JudgmentRequest batches independent questions that share minimized state.
type JudgmentRequest struct {
	State     string
	Questions []JudgmentQuestion
}

// JudgmentAnswer contains a complete categorical distribution. The selected
// choice or yes/no value is the highest-probability option. A score may be
// fractional within the rubric's range, as reported by the provider.
// Score labels are decimal integers; yes/no labels are "yes" and "no".
type JudgmentAnswer struct {
	ID            string
	Probabilities map[string]float64
	Confidence    float64
	Choice        string
	Score         *float64
	Yes           *bool
}

// JudgmentUsage keeps each unknown field distinct from a reported zero.
// CostUSD is provider-reported; no price-based estimate is substituted.
type JudgmentUsage struct {
	InputTokens  *int64   `json:"input_tokens"`
	OutputTokens *int64   `json:"output_tokens"`
	CostUSD      *float64 `json:"cost_usd"`
}

// JudgmentResponse is a provider's single attempt, including partial accounting
// when the typed answer is unusable. ModelVersion is the resolved version.
type JudgmentResponse struct {
	Answers      []JudgmentAnswer
	ModelVersion string
	Usage        JudgmentUsage
}

// JudgmentFallback is a sanitized reason safe for logs and durable records.
type JudgmentFallback string

// Fallback reasons deliberately contain no provider error or answer text.
const (
	JudgmentDisabled          JudgmentFallback = "disabled"
	JudgmentMissingCredential JudgmentFallback = "missing_credential"
	JudgmentUnavailable       JudgmentFallback = "unavailable"
	JudgmentInvalidResponse   JudgmentFallback = "invalid_response"
	JudgmentInvalidRequest    JudgmentFallback = "invalid_request"
	JudgmentOversize          JudgmentFallback = "request_too_large"
	JudgmentTimeout           JudgmentFallback = "timeout"
	JudgmentBudgetExhausted   JudgmentFallback = "budget_exhausted"
	JudgmentCanceled          JudgmentFallback = "canceled"
	JudgmentRateLimited       JudgmentFallback = "rate_limited"
	JudgmentCooldown          JudgmentFallback = "cooldown"
)

// ErrJudgmentResponse marks terminal, unusable answers. It takes precedence
// over retryable outer errors and never contributes to an outage cooldown.
var ErrJudgmentResponse = errors.New("invalid judgment response")

// ErrJudgmentOversize marks requests refused locally by a provider adapter.
var ErrJudgmentOversize = errors.New("judgment request exceeds provider limits")

// JudgmentResult is a baseline-preserving value, never an error to a worker.
// Attempts counts provider calls; even a zero-attempt fallback is an observation.
type JudgmentResult struct {
	JudgmentResponse
	Fallback JudgmentFallback
	Attempts int
	Duration time.Duration
	Wait     time.Duration
}

// JudgmentStatus is independent of baseline worker readiness.
type JudgmentStatus struct {
	Enabled      bool             `json:"enabled"`
	Healthy      bool             `json:"healthy"`
	LastFallback JudgmentFallback `json:"last_fallback,omitempty"`
	Observations uint64           `json:"observations"`
	Attempts     uint64           `json:"attempts"`
	Fallbacks    uint64           `json:"fallbacks"`
}

// Judgments is a separate typed capability, never a Completer or Embedder.
// A caller deadline bounds all attempts and waits, including shared job budgets.
type Judgments interface {
	Judge(context.Context, JudgmentRequest) JudgmentResult
	Choice(context.Context, string, JudgmentQuestion) JudgmentResult
	Score(context.Context, string, JudgmentQuestion) JudgmentResult
	YesNo(context.Context, string, JudgmentQuestion) JudgmentResult
	Status() JudgmentStatus
}

// JudgmentProvider makes exactly one context-bound attempt. Providers must
// honor cancellation and return sanitized errors, never raw transport bodies.
type JudgmentProvider interface {
	Judge(context.Context, JudgmentRequest) (JudgmentResponse, error)
}

// JudgmentFactory constructs the optional provider, only after enablement and
// credential resolution. Keeping construction lazy makes disabled operation inert.
type JudgmentFactory func(JudgmentConfig, string) JudgmentProvider

type judgments struct {
	cfg       JudgmentConfig
	provider  JudgmentProvider
	mu        sync.Mutex
	status    JudgmentStatus
	failures  int
	until     time.Time
	permanent JudgmentFallback
}

// NewJudgments builds one shared capability. Disabled operation neither reads
// credentials nor constructs a client. Missing credentials degrade rather than
// failing startup. getenv is injectable; pass os.Getenv in production.
func NewJudgments(cfg JudgmentConfig, factory JudgmentFactory, getenv func(string) string) (Judgments, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	j := &judgments{cfg: cfg.WithDefaults(), status: JudgmentStatus{Enabled: cfg.Enabled, Healthy: true}}
	if !cfg.Enabled {
		j.permanent = JudgmentDisabled
		return j, nil
	}
	key := ""
	if getenv != nil {
		key = getenv(j.cfg.APIKeyEnv)
	}
	if key == "" {
		j.permanent = JudgmentMissingCredential
	} else if factory == nil {
		j.permanent = JudgmentUnavailable
	} else {
		j.provider = factory(j.cfg, key)
		if j.provider == nil {
			j.permanent = JudgmentUnavailable
		}
	}
	if j.permanent != "" {
		j.status.Healthy = false
		j.status.LastFallback = j.permanent
	}
	return j, nil
}

func (j *judgments) Status() JudgmentStatus { j.mu.Lock(); defer j.mu.Unlock(); return j.status }
func (j *judgments) Choice(ctx context.Context, state string, q JudgmentQuestion) JudgmentResult {
	q.Kind = JudgmentChoice
	return j.Judge(ctx, JudgmentRequest{State: state, Questions: []JudgmentQuestion{q}})
}
func (j *judgments) Score(ctx context.Context, state string, q JudgmentQuestion) JudgmentResult {
	q.Kind = JudgmentScore
	return j.Judge(ctx, JudgmentRequest{State: state, Questions: []JudgmentQuestion{q}})
}
func (j *judgments) YesNo(ctx context.Context, state string, q JudgmentQuestion) JudgmentResult {
	q.Kind = JudgmentYesNo
	return j.Judge(ctx, JudgmentRequest{State: state, Questions: []JudgmentQuestion{q}})
}

func (j *judgments) Judge(parent context.Context, req JudgmentRequest) (out JudgmentResult) {
	// No provider attempt means known zero consumption. The first attempted
	// response replaces these values, including unknown fields.
	zeroTokens, zeroCost := int64(0), float64(0)
	out.Usage = JudgmentUsage{InputTokens: &zeroTokens, OutputTokens: &zeroTokens, CostUSD: &zeroCost}
	start := time.Now()
	defer func() { out.Duration = time.Since(start); j.observe(out) }()
	if j.permanent != "" {
		out.Fallback = j.permanent
		return
	}
	if parent.Err() != nil {
		out.Fallback = parentFallback(parent)
		return
	}
	j.mu.Lock()
	cooling := time.Now().Before(j.until)
	j.mu.Unlock()
	if cooling {
		out.Fallback = JudgmentCooldown
		return
	}
	if !ValidJudgmentRequest(req) {
		out.Fallback = JudgmentInvalidRequest
		return
	}
	ctx, cancel := context.WithTimeout(parent, j.cfg.TotalTimeout)
	defer cancel()
	for attempt := 0; attempt <= j.cfg.Retries(); attempt++ {
		if ctx.Err() != nil {
			out.Fallback = deadlineFallback(parent)
			return
		}
		ac, stop := context.WithTimeout(ctx, j.cfg.Timeout)
		response, err := j.provider.Judge(ac, req)
		attemptErr := ac.Err()
		stop()
		out.Attempts++
		usageOK := SanitizeJudgmentUsage(&response.Usage)
		if out.Attempts == 1 {
			out.Usage = response.Usage
		} else {
			out.Usage = AddJudgmentUsage(out.Usage, response.Usage)
		}
		out.ModelVersion = response.ModelVersion
		// Terminal marker precedence is essential: an outer retryable error must
		// not turn malformed answers into retries or outage observations.
		if !usageOK || errors.Is(err, ErrJudgmentResponse) {
			out.Fallback = JudgmentInvalidResponse
			return
		}
		if errors.Is(err, ErrJudgmentOversize) {
			out.Fallback = JudgmentOversize
			return
		}
		if ctx.Err() != nil {
			out.Fallback = deadlineFallback(parent)
			return
		}
		if err == nil && attemptErr == nil {
			if !ValidJudgmentResponse(req, response) {
				out.Fallback = JudgmentInvalidResponse
				return
			}
			out.Answers = response.Answers
			out.Fallback = ""
			return
		}
		retry := Retryable(err) || errors.Is(attemptErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
		out.Fallback = JudgmentUnavailable
		var pe *Error
		var delay time.Duration
		if errors.As(err, &pe) {
			delay = pe.RetryAfter
			if pe.Status == 429 || pe.Status == 529 {
				out.Fallback = JudgmentRateLimited
				retry = true
			}
		}
		if errors.Is(attemptErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			out.Fallback = JudgmentTimeout
		}
		if !retry || attempt == j.cfg.Retries() {
			return
		}
		backoff := j.cfg.Backoff * time.Duration(1<<attempt)
		if delay < backoff {
			delay = backoff
		}
		waitStart := time.Now()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			out.Wait += time.Since(waitStart)
			out.Fallback = deadlineFallback(parent)
			return
		case <-timer.C:
			out.Wait += time.Since(waitStart)
		}
	}
	return
}

func parentFallback(ctx context.Context) JudgmentFallback {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return JudgmentBudgetExhausted
	}
	return JudgmentCanceled
}
func deadlineFallback(parent context.Context) JudgmentFallback {
	if parent.Err() != nil {
		return parentFallback(parent)
	}
	if deadline, ok := parent.Deadline(); ok && !time.Now().Before(deadline) {
		return JudgmentBudgetExhausted
	}
	return JudgmentTimeout
}

func (j *judgments) observe(out JudgmentResult) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.Observations++
	j.status.Attempts += uint64(out.Attempts)
	if out.Fallback == "" {
		j.failures = 0
		// Another in-flight request may have opened the cooldown. Recovery
		// must not shorten that shared pause.
		j.status.Healthy = !time.Now().Before(j.until)
		j.status.LastFallback = ""
		return
	}
	j.status.Fallbacks++
	j.status.LastFallback = out.Fallback
	if out.Fallback == JudgmentDisabled {
		return
	}
	j.status.Healthy = false
	switch out.Fallback {
	case JudgmentUnavailable, JudgmentTimeout, JudgmentRateLimited:
		j.failures++
		if j.failures >= j.cfg.CooldownFailures {
			j.until = time.Now().Add(j.cfg.Cooldown)
		}
	}
}

// JudgmentOptions returns the labels a question's distribution must cover.
func JudgmentOptions(q JudgmentQuestion) []string {
	switch q.Kind {
	case JudgmentYesNo:
		return []string{"yes", "no"}
	case JudgmentScore:
		if q.Min < -100 || q.Max > 100 || q.Min >= q.Max || q.Max-q.Min > 9 {
			return nil
		}
		out := make([]string, 0, q.Max-q.Min+1)
		for n := q.Min; n <= q.Max; n++ {
			out = append(out, strconv.Itoa(n))
		}
		return out
	case JudgmentChoice:
		return q.Options
	}
	return nil
}

// ValidJudgmentRequest checks only primitive structure, never domain policy.
func ValidJudgmentRequest(req JudgmentRequest) bool {
	if req.State == "" || len(req.Questions) == 0 || len(req.Questions) > 100 {
		return false
	}
	ids := map[string]bool{}
	for _, q := range req.Questions {
		if q.ID == "" || ids[q.ID] || q.Question == "" {
			return false
		}
		ids[q.ID] = true
		opts := JudgmentOptions(q)
		if len(opts) < 2 || len(opts) > 255 {
			return false
		}
		if q.Kind == JudgmentScore && q.Rubric == "" {
			return false
		}
		seen := map[string]bool{}
		for _, o := range opts {
			if o == "" || seen[o] {
				return false
			}
			seen[o] = true
		}
	}
	return true
}

func probability(p float64) bool { return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1 }

// ValidJudgmentResponse also validates injected providers; adapter validation
// alone would let a fake or a future provider bypass the fallback contract.
func ValidJudgmentResponse(req JudgmentRequest, r JudgmentResponse) bool {
	if r.ModelVersion == "" || len(r.ModelVersion) > 256 || len(req.Questions) != len(r.Answers) {
		return false
	}
	answers := map[string]JudgmentAnswer{}
	for _, a := range r.Answers {
		if _, ok := answers[a.ID]; ok {
			return false
		}
		answers[a.ID] = a
	}
	for _, q := range req.Questions {
		a, ok := answers[q.ID]
		if !ok || !probability(a.Confidence) {
			return false
		}
		opts := JudgmentOptions(q)
		if len(opts) != len(a.Probabilities) {
			return false
		}
		sum := 0.0
		maxp := 0.0
		for _, o := range opts {
			p, exists := a.Probabilities[o]
			if !exists || !probability(p) {
				return false
			}
			sum += p
			maxp = math.Max(maxp, p)
		}
		if math.Abs(sum-1) > 1e-6 {
			return false
		}
		selected := ""
		switch q.Kind {
		case JudgmentChoice:
			if a.Score != nil || a.Yes != nil {
				return false
			}
			selected = a.Choice
		case JudgmentScore:
			if a.Score == nil || math.IsNaN(*a.Score) || *a.Score < float64(q.Min) || *a.Score > float64(q.Max) || a.Yes != nil || a.Choice != "" {
				return false
			}
			continue
		case JudgmentYesNo:
			if a.Yes == nil || a.Score != nil || a.Choice != "" {
				return false
			}
			selected = "no"
			if *a.Yes {
				selected = "yes"
			}
		}
		p, exists := a.Probabilities[selected]
		if !exists || p+1e-6 < maxp {
			return false
		}
	}
	return true
}

// SanitizeJudgmentUsage clears invalid fields while retaining valid partial
// accounting. False means the answer must be refused, including reported negatives.
func SanitizeJudgmentUsage(u *JudgmentUsage) bool {
	valid := true
	if u.InputTokens != nil && *u.InputTokens < 0 {
		u.InputTokens = nil
		valid = false
	}
	if u.OutputTokens != nil && *u.OutputTokens < 0 {
		u.OutputTokens = nil
		valid = false
	}
	if u.CostUSD != nil && (math.IsNaN(*u.CostUSD) || math.IsInf(*u.CostUSD, 0) || *u.CostUSD < 0) {
		u.CostUSD = nil
		valid = false
	}
	return valid
}

// AddJudgmentUsage propagates unknowns and overflow separately per field.
func AddJudgmentUsage(a, b JudgmentUsage) JudgmentUsage {
	add := func(a, b *int64) *int64 {
		if a == nil || b == nil || *a < 0 || *b < 0 || *a > math.MaxInt64-*b {
			return nil
		}
		n := *a + *b
		return &n
	}
	out := JudgmentUsage{InputTokens: add(a.InputTokens, b.InputTokens), OutputTokens: add(a.OutputTokens, b.OutputTokens)}
	if a.CostUSD != nil && b.CostUSD != nil && *a.CostUSD >= 0 && *b.CostUSD >= 0 {
		n := *a.CostUSD + *b.CostUSD
		if !math.IsNaN(n) && !math.IsInf(n, 0) {
			out.CostUSD = &n
		}
	}
	return out
}
