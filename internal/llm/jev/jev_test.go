package jev_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/jev"
	"github.com/kpenfound/hearsay/internal/llm/providers"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func request() llm.JudgmentRequest {
	return llm.JudgmentRequest{State: "PRIVATE STATE", Questions: []llm.JudgmentQuestion{{ID: "q", Kind: llm.JudgmentChoice, Question: "PRIVATE QUESTION", Options: []string{"z", "a"}}}}
}
func judgments(t *testing.T, s *httptest.Server, cfg llm.JudgmentConfig) llm.Judgments {
	t.Helper()
	cfg.Enabled = true
	cfg.Model = "configured-model"
	cfg.BaseURL = s.URL
	j, err := llm.NewJudgments(cfg, func(c llm.JudgmentConfig, key string) llm.JudgmentProvider { return jev.New(c, key) }, func(string) string { return "SECRET KEY" })
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestAdapterBatchWireContract(t *testing.T) {
	body := fixture(t, "batch")
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer SECRET KEY" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("transport contract mismatch")
		}
		var b struct {
			State, Model string
			Questions    map[string]struct {
				Type, Instructions string
				Criteria           json.RawMessage
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		if b.State != "PRIVATE STATE" || b.Model != "configured-model" || len(b.Questions) != 3 {
			t.Error("shared state or explicit model lost")
		}
		if string(b.Questions["q"].Criteria) != `{"z":"z","a":"a"}` {
			t.Errorf("choice order: %s", b.Questions["q"].Criteria)
		}
		if b.Questions["score"].Type != "score" || string(b.Questions["score"].Criteria) != `["1","2","3"]` || !strings.Contains(b.Questions["score"].Instructions, "quality rubric") {
			t.Error("score wire contract mismatch")
		}
		if b.Questions["boolean"].Type != "noul" {
			t.Error("yes/no must use noul")
		}
		fmt.Fprint(w, body)
	}))
	defer s.Close()
	r := request()
	r.Questions = append(r.Questions, llm.JudgmentQuestion{ID: "score", Kind: llm.JudgmentScore, Question: "rate", Rubric: "quality rubric", Min: 1, Max: 3}, llm.JudgmentQuestion{ID: "boolean", Kind: llm.JudgmentYesNo, Question: "true?"})
	out := judgments(t, s, llm.JudgmentConfig{}).Judge(t.Context(), r)
	if out.Fallback != "" || calls.Load() != 1 || out.ModelVersion != "jev-1.13.0" || len(out.Answers) != 3 {
		t.Fatalf("result=%+v calls=%d", out, calls.Load())
	}
	if *out.Answers[1].Score != 2.25 || out.Answers[1].Probabilities["2"] != 0.7 || !*out.Answers[2].Yes || out.Answers[2].Probabilities["no"] != 0.25 || out.Answers[2].Confidence != 0.75 {
		t.Fatalf("typed answers=%+v", out.Answers)
	}
	if out.Usage.InputTokens == nil || *out.Usage.InputTokens != 12 || out.Usage.OutputTokens == nil || *out.Usage.OutputTokens != 0 || out.Usage.CostUSD != nil {
		t.Fatalf("usage=%+v", out.Usage)
	}
}

func TestAdapterInvalidResponseRetainsAccountingWithoutCooldown(t *testing.T) {
	good := fixture(t, "choice")
	for _, tt := range []struct {
		name, body          string
		input, output, cost bool
	}{
		{"malformed", `not JSON PRIVATE ANSWER`, false, false, false},
		{"truncated with complete usage prefix", `{"usage":{"input_tokens":12,"cost":0},"answers":`, false, false, false},
		{"trailing JSON", good + `{}`, false, false, false},
		{"null probability", strings.Replace(good, `"z":0.8,"a":0.2`, `"z":1,"a":null`, 1), true, true, true},
		{"missing probability", strings.Replace(good, `,"a":0.2`, "", 1), true, true, true},
		{"missing probabilities", strings.Replace(good, `"probabilities":{"z":0.8,"a":0.2},`, "", 1), true, true, true},
		{"null distribution", strings.Replace(good, `{"z":0.8,"a":0.2}`, `null`, 1), true, true, true},
		{"string probability", strings.Replace(good, `"a":0.2`, `"a":"0.2"`, 1), true, true, true},
		{"non normalized", strings.Replace(good, `"z":0.8`, `"z":0.3`, 1), true, true, true},
		{"null confidence", strings.Replace(good, `"confidence":0.8`, `"confidence":null`, 1), true, true, true},
		{"missing confidence", strings.Replace(good, `,"confidence":0.8`, "", 1), true, true, true},
		{"wrong answer type", strings.Replace(good, `"type":"choice"`, `"type":"score"`, 1), true, true, true},
		{"wrong choice type", strings.Replace(good, `"choice":"z"`, `"choice":7`, 1), true, true, true},
		{"wrong selected choice", strings.Replace(good, `"choice":"z"`, `"choice":"a"`, 1), true, true, true},
		{"missing answer", strings.Replace(good, `"q":`, `"other":`, 1), true, true, true},
		{"wrong model type", strings.Replace(good, `"model":"jev-1.13.0"`, `"model":3`, 1), true, true, true},
		{"missing model", strings.Replace(good, `"model":"jev-1.13.0",`, "", 1), true, true, true},
		{"negative input", strings.Replace(good, `"input_tokens":12`, `"input_tokens":-1`, 1), false, true, true},
		{"overflow input", strings.Replace(good, `"input_tokens":12`, `"input_tokens":9223372036854775808`, 1), false, true, true},
		{"negative output", strings.Replace(good, `"output_tokens":0`, `"output_tokens":-1`, 1), true, false, true},
		{"fractional output", strings.Replace(good, `"output_tokens":0`, `"output_tokens":0.5`, 1), true, false, true},
		{"null input", strings.Replace(good, `"input_tokens":12`, `"input_tokens":null`, 1), false, true, true},
		{"string input", strings.Replace(good, `"input_tokens":12`, `"input_tokens":"12"`, 1), false, true, true},
		{"negative cost", strings.Replace(good, `"cost":0`, `"cost":-1`, 1), true, true, false},
		{"overflow cost", strings.Replace(good, `"cost":0`, `"cost":1e999`, 1), true, true, false},
		{"null cost", strings.Replace(good, `"cost":0`, `"cost":null`, 1), true, true, false},
		{"oversize response", strings.Repeat("x", (1<<20)+1), false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) <= 3 {
					fmt.Fprint(w, tt.body)
				} else {
					fmt.Fprint(w, good)
				}
			}))
			defer s.Close()
			j := judgments(t, s, llm.JudgmentConfig{CooldownFailures: 1})
			for range 2 {
				out := j.Judge(t.Context(), request())
				if out.Fallback != llm.JudgmentInvalidResponse || out.Attempts != 1 || out.Wait != 0 || len(out.Answers) != 0 {
					t.Fatalf("invalid response=%+v", out)
				}
				if (out.Usage.InputTokens != nil) != tt.input || (out.Usage.OutputTokens != nil) != tt.output || (out.Usage.CostUSD != nil) != tt.cost {
					t.Fatalf("partial usage=%+v", out.Usage)
				}
			}
			_, err := jev.New(llm.JudgmentConfig{Model: "explicit", BaseURL: s.URL}, "key").Judge(t.Context(), request())
			if !errors.Is(err, llm.ErrJudgmentResponse) || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("unsafe error: %v", err)
			}
			out := j.Judge(t.Context(), request())
			if out.Fallback != "" || calls.Load() != 4 || !j.Status().Healthy {
				t.Fatalf("invalid answer contributed to cooldown: %+v", out)
			}
		})
	}
}

func TestAdapterAccountingUnknownVersusZero(t *testing.T) {
	good := fixture(t, "choice")
	for _, tt := range []struct {
		name, body          string
		input, output, cost bool
	}{
		{"reported zero cost", good, true, true, true},
		{"no cost", strings.Replace(good, `,"cost":0`, "", 1), true, true, false},
		{"no input", strings.Replace(good, `"input_tokens":12,`, "", 1), false, true, true},
		{"no output", strings.Replace(good, `"output_tokens":0,`, "", 1), true, false, true},
		{"no accounting", strings.Replace(good, `,"usage":{"input_tokens":12,"output_tokens":0,"cost":0}`, "", 1), false, false, false},
		{"numeric zero probability", strings.Replace(good, `"z":0.8,"a":0.2`, `"z":1,"a":0`, 1), true, true, true},
		{"rounded distribution", strings.Replace(good, `"z":0.8`, `"z":0.79`, 1), true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tt.body) }))
			defer s.Close()
			out := judgments(t, s, llm.JudgmentConfig{}).Judge(t.Context(), request())
			if out.Fallback != "" || (out.Usage.InputTokens != nil) != tt.input || (out.Usage.OutputTokens != nil) != tt.output || (out.Usage.CostUSD != nil) != tt.cost {
				t.Fatalf("result=%+v", out)
			}
			if out.Usage.CostUSD != nil && *out.Usage.CostUSD != 0 {
				t.Fatal("cost must be reported zero")
			}
		})
	}
}

func TestAdapterHTTPStatusesAndRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		status int
		kind   string
		retry  bool
	}{
		{429, "rate_limited", true}, {529, "rate_limited", true}, {500, "unavailable", true}, {503, "unavailable", true}, {408, "unavailable", true}, {401, "unauthorized", false}, {403, "unauthorized", false}, {400, "rejected", false}, {422, "rejected", false}, {302, "rejected", false},
	} {
		for _, header := range []string{"2", time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat), "999999999999999999", "invalid", "-1"} {
			t.Run(fmt.Sprintf("%d/%s", tt.status, header), func(t *testing.T) {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Retry-After", header)
					w.WriteHeader(tt.status)
					fmt.Fprint(w, "PRIVATE ANSWER SECRET KEY")
				}))
				defer s.Close()
				_, err := jev.New(llm.JudgmentConfig{Model: "explicit", BaseURL: s.URL}, "SECRET KEY").Judge(t.Context(), request())
				var e *llm.Error
				if !errors.As(err, &e) || e.Status != tt.status || e.Kind != tt.kind || e.Retryable != tt.retry {
					t.Fatalf("error=%v", err)
				}
				if strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("unsafe error=%v", err)
				}
				if tt.kind == "rate_limited" && header != "invalid" && header != "-1" && e.RetryAfter < time.Second {
					t.Fatalf("Retry-After lost: %v", e.RetryAfter)
				}
			})
		}
	}
}

func TestAdapterRefusesOversizeBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"state plus question", "total", "configured limit"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer s.Close()
			r := request()
			cfg := llm.JudgmentConfig{}
			switch mode {
			case "state plus question":
				r.State = strings.Repeat("s", 32000)
			case "total":
				r.Questions = nil
				for i := range 30 {
					q := request().Questions[0]
					q.ID = fmt.Sprint(i)
					q.Question = strings.Repeat("q", 2400)
					r.Questions = append(r.Questions, q)
				}
			case "configured limit":
				cfg.MaxTotalTokens = 500
				cfg.MaxStateQuestionTokens = 200
				r.State = strings.Repeat("s", 200)
			}
			out := judgments(t, s, cfg).Judge(t.Context(), r)
			if out.Fallback != llm.JudgmentOversize || calls.Load() != 0 {
				t.Fatalf("result=%+v calls=%d", out, calls.Load())
			}
		})
	}
}

func TestAdapterRetryRecoveryAndDeadlines(t *testing.T) {
	good := fixture(t, "choice")
	for _, mode := range []string{"retry recovery", "attempt timeout", "total timeout", "caller deadline", "retry after budget"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				io.Copy(io.Discard, r.Body)
				if mode == "retry after budget" {
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(529)
					return
				}
				if mode == "retry recovery" && n == 1 {
					w.WriteHeader(503)
					return
				}
				if mode == "total timeout" || mode == "caller deadline" || mode == "attempt timeout" && n == 1 {
					<-r.Context().Done()
					return
				}
				fmt.Fprint(w, good)
			}))
			defer s.Close()
			// Recovery needs headroom under race instrumentation and parallel load.
			// Only the cases testing exhaustion get a short total or caller budget.
			cfg := llm.JudgmentConfig{Backoff: time.Millisecond, Timeout: 20 * time.Millisecond, TotalTimeout: time.Second, MaxRetries: 5}
			ctx := t.Context()
			want := llm.JudgmentFallback("")
			attempts := 2
			if mode == "total timeout" {
				cfg.TotalTimeout = 40 * time.Millisecond
				want = llm.JudgmentTimeout
				attempts = -1 // The total deadline may cut off any retry.
			}
			if mode == "caller deadline" || mode == "retry after budget" {
				cfg.Timeout = time.Second
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				want = llm.JudgmentBudgetExhausted
				attempts = 1
			}
			j := judgments(t, s, cfg)
			start := time.Now()
			out := j.Judge(ctx, request())
			if out.Fallback != want || (attempts >= 0 && out.Attempts != attempts) {
				t.Fatalf("result=%+v want fallback=%s attempts=%d", out, want, attempts)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatalf("result=%+v elapsed=%v", out, time.Since(start))
			}
		})
	}
}

func TestAdapterDoesNotFollowRedirect(t *testing.T) {
	var received atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Add(1) }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer s.Close()
	out := judgments(t, s, llm.JudgmentConfig{}).Judge(t.Context(), request())
	if received.Load() != 0 || out.Attempts != 1 || out.Fallback != llm.JudgmentUnavailable {
		t.Fatalf("redirect followed: %+v calls=%d", out, received.Load())
	}
}

func TestAdapterScoreAndBooleanValidation(t *testing.T) {
	good := fixture(t, "batch")
	for _, tt := range []struct{ name, from, to string }{
		{"null score", `"score":1.25`, `"score":null`},
		{"missing score", `"score":1.25,`, ``},
		{"score below rubric", `"score":1.25`, `"score":-0.1`},
		{"score above rubric", `"score":1.25`, `"score":2.1`},
		{"wrong score probability keys", `"0":0.1`, `"3":0.1`},
		{"null score probability", `"0":0.1,"1":0.7,"2":0.2`, `"0":null,"1":0.8,"2":0.2`},
		{"null noul", `"noul":0.75`, `"noul":null`},
		{"missing noul", `,"noul":0.75`, ``},
		{"wrong noul type", `"noul":0.75`, `"noul":"0.75"`},
		{"noul out of range", `"noul":0.75`, `"noul":1.01`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.Replace(good, tt.from, tt.to, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
			defer s.Close()
			r := request()
			r.Questions = append(r.Questions, llm.JudgmentQuestion{ID: "score", Kind: llm.JudgmentScore, Question: "rate", Rubric: "rubric", Min: 1, Max: 3}, llm.JudgmentQuestion{ID: "boolean", Kind: llm.JudgmentYesNo, Question: "true?"})
			out := judgments(t, s, llm.JudgmentConfig{CooldownFailures: 1}).Judge(t.Context(), r)
			if out.Fallback != llm.JudgmentInvalidResponse || out.Attempts != 1 || len(out.Answers) != 0 || out.Usage.InputTokens == nil {
				t.Fatalf("result=%+v", out)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type unreadableBody struct{ reads *int }

func (b unreadableBody) Read([]byte) (int, error) { *b.reads++; return 0, errors.New("PRIVATE ANSWER") }
func (unreadableBody) Close() error               { return nil }

func TestAdapterClassifiesRateLimitBeforeBodyIO(t *testing.T) {
	for _, status := range []int{200, 429, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			reads := 0
			client := jev.New(llm.JudgmentConfig{Model: "explicit"}, "key")
			client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"2"}}, Body: unreadableBody{&reads}}, nil
			})}
			_, err := client.Judge(t.Context(), request())
			var e *llm.Error
			if !errors.As(err, &e) || !e.Retryable || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("error=%v", err)
			}
			if status == 200 {
				if reads != 1 || e.Kind != "unavailable" {
					t.Fatalf("reads=%d error=%v", reads, err)
				}
			} else if reads != 0 || e.RetryAfter != 2*time.Second || e.Kind != "rate_limited" {
				t.Fatalf("reads=%d error=%v", reads, err)
			}
		})
	}
}

func TestJevIsAbsentFromTierRegistry(t *testing.T) {
	if _, ok := providers.ByName("jev"); ok {
		t.Fatal("Jev registered as a generative provider")
	}
	for _, tier := range llm.Tiers {
		cfg := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{tier: {Provider: "jev", Model: "explicit"}}}
		if tier == llm.TierEmbed {
			cfg.Tiers[tier] = llm.TierConfig{Provider: "jev", Model: "explicit", Dimensions: 1024}
		}
		if _, err := llm.NewRegistry(cfg, providers.All()); err == nil || !strings.Contains(err.Error(), "no provider called") {
			t.Fatalf("Jev reachable as tier %s: %v", tier, err)
		}
	}
}
