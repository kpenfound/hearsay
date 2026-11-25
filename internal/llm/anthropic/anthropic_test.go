package anthropic_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/anthropic"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

type recorded struct {
	status  int
	file    string
	headers map[string]string
}

func replay(t *testing.T, answers ...recorded) (*httptest.Server, *[]request) {
	t.Helper()
	var seen []request
	i := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("the adapter sent something that is not JSON: %v", err)
		}
		seen = append(seen, request{path: r.URL.Path, header: r.Header.Clone(), body: body})
		if i >= len(answers) {
			t.Errorf("the adapter made %d requests and %d were recorded", i+1, len(answers))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		answer := answers[i]
		i++
		for k, v := range answer.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(answer.status)
		file, err := os.ReadFile(filepath.Join("testdata", answer.file))
		if err != nil {
			t.Errorf("reading the recording: %v", err)
			return
		}
		_, _ = w.Write(file)
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

type request struct {
	path   string
	header http.Header
	body   map[string]any
}

func registry(t *testing.T, url string, tiers map[llm.Tier]llm.TierConfig) llm.Registry {
	t.Helper()
	config := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{}}
	for tier, tc := range tiers {
		tc.BaseURL = url
		tc.Backoff = time.Millisecond
		config.Tiers[tier] = tc
	}
	r, err := llm.NewRegistry(config, []llm.Provider{anthropic.New()},
		llm.WithEnv(func(name string) string {
			if name == anthropic.APIKeyEnv {
				return "sk-ant-test"
			}
			return ""
		}))
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	return r
}

const distillSchema = `{
  "type": "object",
  "properties": {
    "summary": {"type": "string"},
    "outcome_kind": {"type": "string", "enum": ["decided", "proposed", "resolved", "informational", "open"]}
  },
  "required": ["summary", "outcome_kind"],
  "additionalProperties": false
}`

func TestCompletionTiers(t *testing.T) {
	server, seen := replay(t,
		recorded{status: 200, file: "text_answer.json"},
		recorded{status: 200, file: "assert_answer.json"})
	r := registry(t, server.URL, map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5", MaxTokens: 1024},
		llm.TierAssert:  {Provider: "anthropic", Model: "claude-sonnet-5"},
	})

	distill, err := r.Completer(llm.TierDistill)
	if err != nil {
		t.Fatalf("Completer(distill) = %v", err)
	}
	resp, err := distill.Complete(t.Context(), llm.Request{
		System:   "You distil one artifact into one document.",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "What did the team decide about the queue?"}},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if want := "They decided to put the job queue in Postgres."; resp.Text != want {
		t.Errorf("Text = %q, want %q", resp.Text, want)
	}
	if resp.StopReason != llm.StopEnd {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, llm.StopEnd)
	}
	if resp.Tokens != (llm.Tokens{Input: 88, Output: 12}) {
		t.Errorf("Tokens = %+v, want the recording's", resp.Tokens)
	}
	if resp.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want the model the answer says answered, not the alias that was asked for", resp.Model)
	}

	assert, err := r.Completer(llm.TierAssert)
	if err != nil {
		t.Fatalf("Completer(assert) = %v", err)
	}
	resp, err = assert.Complete(t.Context(), llm.Request{
		System:   "You extract one stance and the topic it is about.",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "The job queue moves to Postgres."}},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if !strings.HasPrefix(resp.Text, "stance:") {
		t.Errorf("Text = %q, want the assert tier's recorded answer", resp.Text)
	}

	if len(*seen) != 2 {
		t.Fatalf("the adapter made %d requests, want 2", len(*seen))
	}
	first := (*seen)[0]
	if first.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", first.path)
	}
	if got := first.header.Get("x-api-key"); got != "sk-ant-test" {
		t.Errorf("x-api-key = %q, want the credential from the environment", got)
	}
	if got := first.header.Get("anthropic-version"); got == "" {
		t.Error("the request carries no anthropic-version header")
	}
	if got := first.body["model"]; got != "claude-haiku-4-5" {
		t.Errorf("model = %v, want the tier's, passed through untouched", got)
	}
	if got := first.body["max_tokens"]; got != float64(1024) {
		t.Errorf("max_tokens = %v, want the tier's budget", got)
	}
	if got := first.body["system"]; got != "You distil one artifact into one document." {
		t.Errorf("system = %v, want the system prompt", got)
	}

	if got := (*seen)[1].body["max_tokens"]; got != float64(llm.DefaultMaxTokens) {
		t.Errorf("the assert tier's max_tokens = %v, want the default %d", got, llm.DefaultMaxTokens)
	}

	if got, ok := first.body["temperature"]; ok {
		t.Errorf("temperature = %v, want the field left out where the tier does not set one", got)
	}
}

func TestTemperatureZeroIsSent(t *testing.T) {
	zero := 0.0
	server, seen := replay(t, recorded{status: 200, file: "assert_answer.json"})
	r := registry(t, server.URL, map[llm.Tier]llm.TierConfig{
		llm.TierAssert: {Provider: "anthropic", Model: "claude-sonnet-5", Temperature: &zero},
	})
	completer, _ := r.Completer(llm.TierAssert)
	if _, err := completer.Complete(t.Context(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "extract the stance"}}}); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	got, ok := (*seen)[0].body["temperature"]
	if !ok || got != float64(0) {
		t.Errorf("temperature = %v (present: %v), want the 0 the tier configures", got, ok)
	}
}

func TestTheRegistrysHTTPClientIsUsed(t *testing.T) {
	used := false
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		body, err := os.ReadFile(filepath.Join("testdata", "text_answer.json"))
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)),
			Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
	})}
	config := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
	}}
	r, err := llm.NewRegistry(config, []llm.Provider{anthropic.New()},
		llm.WithEnv(func(string) string { return "sk-ant-test" }), llm.WithHTTPClient(client))
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	completer, _ := r.Completer(llm.TierDistill)
	resp, err := completer.Complete(t.Context(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "hello"}}})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if !used {
		t.Error("the request did not go through the client the registry was built with")
	}
	if resp.Text == "" {
		t.Error("Complete() returned no text")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStructuredOutput(t *testing.T) {
	server, seen := replay(t, recorded{status: 200, file: "tool_use_answer.json"})
	r := registry(t, server.URL, map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
	})
	completer, _ := r.Completer(llm.TierDistill)
	resp, err := completer.Complete(t.Context(), llm.Request{
		System:   "You distil one artifact into one document.",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "PR #62 merged."}},
		Schema: &llm.Schema{Name: "distillation", Description: "one L1 document",
			Definition: json.RawMessage(distillSchema)},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	var answer struct {
		Summary     string `json:"summary"`
		OutcomeKind string `json:"outcome_kind"`
	}
	if err := json.Unmarshal(resp.JSON, &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if answer.OutcomeKind != "decided" {
		t.Errorf("outcome_kind = %q, want decided", answer.OutcomeKind)
	}
	if resp.Text != "" {
		t.Errorf("Text = %q, want a structured answer to carry no text", resp.Text)
	}

	body := (*seen)[0].body
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want the schema as one tool", body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "distillation" {
		t.Errorf("the tool is called %v, want the schema's name", tool["name"])
	}
	var sent, want any
	_ = json.Unmarshal([]byte(distillSchema), &want)
	sent = tool["input_schema"]
	if !jsonEqual(sent, want) {
		t.Errorf("input_schema = %v, want the caller's schema unchanged", sent)
	}
	choice, ok := body["tool_choice"].(map[string]any)
	if !ok || choice["type"] != "tool" || choice["name"] != "distillation" {
		t.Errorf("tool_choice = %v, want the model required to use the tool", body["tool_choice"])
	}
	if choice["disable_parallel_tool_use"] != true {
		t.Errorf("tool_choice = %v, want parallel tool use disabled so there is one answer to read", choice)
	}
}

func TestAnswerThatUsedAnotherTool(t *testing.T) {
	server, _ := replay(t, recorded{status: 200, file: "tool_use_other_name.json"})
	r := registry(t, server.URL, map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001", MaxRetries: -1},
	})
	completer, _ := r.Completer(llm.TierDistill)
	_, err := completer.Complete(t.Context(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "PR #62 merged."}},
		Schema:   &llm.Schema{Name: "distillation", Definition: json.RawMessage(distillSchema)},
	})
	if err == nil {
		t.Fatal("Complete() = nil, want the answer to be refused")
	}
	if !strings.Contains(err.Error(), "distillation tool") {
		t.Errorf("Complete() = %q, want it to name the tool the answer did not use", err)
	}
}

func TestTruncatedStructuredAnswer(t *testing.T) {
	server, _ := replay(t, recorded{status: 200, file: "truncated_answer.json"})
	r := registry(t, server.URL, map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001", MaxTokens: 16},
	})
	completer, _ := r.Completer(llm.TierDistill)
	_, err := completer.Complete(t.Context(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "PR #62 merged."}},
		Schema:   &llm.Schema{Name: "distillation", Definition: json.RawMessage(distillSchema)},
	})
	if !errors.Is(err, llm.ErrTruncated) {
		t.Fatalf("Complete() = %v, want ErrTruncated", err)
	}
	if !strings.Contains(err.Error(), "16") {
		t.Errorf("Complete() = %q, want it to name the budget that was not enough", err)
	}
}

func TestErrorsAreClassified(t *testing.T) {
	for _, tt := range []struct {
		name          string
		answer        recorded
		wantRetryable bool
		wantAfter     time.Duration
		wantKind      string
		wantMsg       string
	}{
		{
			name:          "rate limited, with a wait the provider asked for",
			answer:        recorded{status: 429, file: "error_rate_limit.json", headers: map[string]string{"retry-after": "2"}},
			wantRetryable: true,
			wantAfter:     2 * time.Second,
			wantKind:      "rate_limit_error",
			wantMsg:       "rate limit",
		},
		{
			name:          "overloaded",
			answer:        recorded{status: 529, file: "error_overloaded.json"},
			wantRetryable: true,
			wantKind:      "overloaded_error",
		},
		{
			name:          "a server error",
			answer:        recorded{status: 500, file: "error_overloaded.json"},
			wantRetryable: true,
		},
		{
			name:     "a request the API will refuse every time",
			answer:   recorded{status: 400, file: "error_invalid_request.json"},
			wantKind: "invalid_request_error",
			wantMsg:  "not a supported model",
		},
		{
			name:   "a credential the API does not accept",
			answer: recorded{status: 401, file: "error_invalid_request.json"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, _ := replay(t, tt.answer)
			r := registry(t, server.URL, map[llm.Tier]llm.TierConfig{
				llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001", MaxRetries: -1},
			})
			completer, _ := r.Completer(llm.TierDistill)
			_, err := completer.Complete(t.Context(), llm.Request{
				Messages: []llm.Message{{Role: llm.RoleUser, Text: "hello"}}})
			if err == nil {
				t.Fatal("Complete() = nil, want the recorded refusal")
			}
			var e *llm.Error
			if !errors.As(err, &e) {
				t.Fatalf("Complete() = %v, want an *llm.Error the retry loop can read", err)
			}
			if e.Retryable != tt.wantRetryable {
				t.Errorf("Retryable = %v, want %v", e.Retryable, tt.wantRetryable)
			}
			if e.RetryAfter != tt.wantAfter {
				t.Errorf("RetryAfter = %v, want %v", e.RetryAfter, tt.wantAfter)
			}
			if e.Status != tt.answer.status {
				t.Errorf("Status = %d, want %d", e.Status, tt.answer.status)
			}
			if tt.wantKind != "" && e.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", e.Kind, tt.wantKind)
			}
			if e.Tier != llm.TierDistill || e.Provider != llm.ProviderAnthropic {
				t.Errorf("the error says %s/%s, want it to name the tier and the provider", e.Tier, e.Provider)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Complete() = %q, want it to carry what the provider said", err)
			}
		})
	}
}

func TestOverloadedIsRetried(t *testing.T) {
	server, seen := replay(t,
		recorded{status: 529, file: "error_overloaded.json"},
		recorded{status: 200, file: "text_answer.json"})
	r := registry(t, server.URL, map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
	})
	completer, _ := r.Completer(llm.TierDistill)
	resp, err := completer.Complete(t.Context(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "hello"}}})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if resp.Text == "" || len(*seen) != 2 {
		t.Errorf("Complete() = %q after %d requests, want the second answer", resp.Text, len(*seen))
	}
}

func TestARequestThatNeverArrived(t *testing.T) {
	server, _ := replay(t)
	url := server.URL
	server.Close()

	r := registry(t, url, map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001", MaxRetries: -1},
	})
	completer, _ := r.Completer(llm.TierDistill)
	_, err := completer.Complete(t.Context(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "hello"}}})
	if err == nil {
		t.Fatal("Complete() = nil, want the call to fail against a server that is not there")
	}
	if !llm.Retryable(err) {
		t.Errorf("Complete() = %q, want a failure the retry loop will try again", err)
	}
	var e *llm.Error
	if !errors.As(err, &e) || e.Status != 0 {
		t.Errorf("Complete() = %v, want an *llm.Error with no HTTP status", err)
	}
}

func TestTheRegistryRefusesAnEmbedTierOnAnthropic(t *testing.T) {
	config := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
		llm.TierEmbed: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001", Dimensions: 1024},
	}}
	_, err := llm.NewRegistry(config, []llm.Provider{anthropic.New()},
		llm.WithEnv(func(string) string { return "sk-ant-test" }))
	if err == nil {
		t.Fatal("NewRegistry() built an embedder on Anthropic")
	}
	if !strings.Contains(err.Error(), "no embedding model") {
		t.Errorf("NewRegistry() = %q, want it to say why", err)
	}
}

func TestAnthropicHasNoEmbedder(t *testing.T) {
	if _, err := anthropic.New().Embedder(llm.Build{Tier: llm.TierEmbed}); err == nil {
		t.Error("Embedder() built one anyway")
	}
}

func TestCredentialComesFromTheEnvironment(t *testing.T) {
	config := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5-20251001"},
	}}
	_, err := llm.NewRegistry(config, []llm.Provider{anthropic.New()}, llm.WithEnv(func(string) string { return "" }))
	if err == nil || !strings.Contains(err.Error(), anthropic.APIKeyEnv) {
		t.Fatalf("NewRegistry() = %v, want it to name %s", err, anthropic.APIKeyEnv)
	}

	config.Tiers[llm.TierDistill] = llm.TierConfig{Provider: "anthropic", Model: "m", APIKeyEnv: "HEARSAY_DISTILL_KEY"}
	if _, err := llm.NewRegistry(config, []llm.Provider{anthropic.New()}, llm.WithEnv(func(name string) string {
		if name == "HEARSAY_DISTILL_KEY" {
			return "sk-ant-other"
		}
		return ""
	})); err != nil {
		t.Errorf("NewRegistry() = %v, want the tier's own variable to be read", err)
	}
}

func completeLogged(t *testing.T, level slog.Level, prompt string) (logs string, answer string) {
	t.Helper()
	server, _ := replay(t, recorded{status: 200, file: "text_answer.json"})
	r := registry(t, server.URL, map[llm.Tier]llm.TierConfig{llm.TierDistill: {Provider: "anthropic", Model: "claude-haiku-4-5"}})
	completer, err := r.Completer(llm.TierDistill)
	if err != nil {
		t.Fatalf("Completer(distill) = %v", err)
	}
	var out bytes.Buffer
	ctx := telemetry.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: level})))
	resp, err := completer.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: prompt}}})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	return out.String(), resp.Text
}

func TestNoPromptOrCompletionIsLoggedAtInfoLevel(t *testing.T) {
	const prompt = "the private question nobody may log"
	logs, answer := completeLogged(t, slog.LevelInfo, prompt)
	if strings.Contains(logs, prompt) || strings.Contains(logs, answer) {
		t.Errorf("info-level output carries content:\n%s", logs)
	}
}

func TestNoCredentialIsLoggedAtDebugLevel(t *testing.T) {
	const prompt = "the private question nobody may log"
	logs, _ := completeLogged(t, slog.LevelDebug, prompt)
	if !strings.Contains(logs, "model call") {
		t.Fatalf("the call was not logged at debug level:\n%s", logs)
	}
	if strings.Contains(logs, "sk-ant-test") {
		t.Errorf("debug-level output carries the credential:\n%s", logs)
	}
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}
