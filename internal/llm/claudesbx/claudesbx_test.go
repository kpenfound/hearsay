package claudesbx_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/claudesbx"
	"github.com/kpenfound/hearsay/internal/llm/claudesbx/claudesbxtest"
)

const leak = "LEAK-"

func fixture(t *testing.T, name string, replace ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(replace...).Replace(string(data))
}

var summarySchema = &llm.Schema{
	Name: "summary",
	Definition: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"topics":{"type":"array","items":{"type":"string"}}},` +
		`"required":["summary"],"additionalProperties":false}`),
}

func user(text string) []llm.Message { return []llm.Message{{Role: llm.RoleUser, Text: text}} }

func request(schema *llm.Schema) llm.Request {
	return llm.Request{System: "You distill team chat.", Messages: user("Why Postgres?"), Schema: schema}
}

func registry(t *testing.T, tiers map[llm.Tier]llm.TierConfig, opts ...llm.Option) (llm.Registry, error) {
	t.Helper()
	cfg := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{}}
	for tier, tc := range tiers {
		tc.Provider = llm.ProviderClaudeSbx
		if tc.Backoff == 0 {
			tc.Backoff = time.Millisecond
		}
		cfg.Tiers[tier] = tc
	}
	return llm.NewRegistry(cfg, []llm.Provider{claudesbx.New()}, opts...)
}

func distill(t *testing.T, tc llm.TierConfig) llm.Completer {
	t.Helper()
	tc.Model = cmpOr(tc.Model, "claude-test-model")
	reg, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierDistill: tc})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	c, err := reg.Completer(llm.TierDistill)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestTheProviderIsCalledClaudeSbx(t *testing.T) {
	if name := claudesbx.New().Name(); name != "claude-sbx" {
		t.Errorf("Name() = %q", name)
	}
}

func TestCapabilitiesCompleteWithASystemPromptAndStructuredOutputButNoEmbedding(t *testing.T) {
	caps := claudesbx.New().Capabilities()
	if !caps.Complete || !caps.SystemPrompt || !caps.StructuredOutput || caps.Embed {
		t.Errorf("Capabilities() = %+v, want complete, system prompt and structured output, and no embedding", caps)
	}
}

func TestCapabilitiesRefuseTemperatureBaseURLAndKeyEnv(t *testing.T) {
	caps := claudesbx.New().Capabilities()
	if !caps.NoTemperature || !caps.NoBaseURL || !caps.NoAPIKeyEnv {
		t.Errorf("Capabilities() = %+v, want temperature, base_url and api_key_env refused", caps)
	}
}

func TestTheDefaultTimeoutIsLongerThanTheRegistrysAndInsideTheCallersBound(t *testing.T) {
	caps := claudesbx.New().Capabilities()
	if caps.DefaultTimeout <= llm.DefaultTimeout {
		t.Errorf("DefaultTimeout = %s, want more than the registry's %s: a call starts a sandbox first", caps.DefaultTimeout, llm.DefaultTimeout)
	}
	if caps.DefaultTimeout >= 2*time.Minute {
		t.Errorf("DefaultTimeout = %s, want less than the two minutes a caller bounds a call to", caps.DefaultTimeout)
	}
}

func TestTheProviderHasNoEmbedder(t *testing.T) {
	if _, err := claudesbx.New().Embedder(llm.Build{}); err == nil || !strings.Contains(err.Error(), "no embedding model") {
		t.Errorf("Embedder() = %v, want it to say the provider has no embedding model", err)
	}
}

func TestRegistryRefusesWhatTheProviderCannotServe(t *testing.T) {
	claudesbxtest.Install(t, "")
	temp := 0.2
	for _, tt := range []struct {
		name string
		tier llm.Tier
		tc   llm.TierConfig
		want []string
	}{
		{"max_tokens and a timeout are taken", llm.TierDistill, llm.TierConfig{Model: "m", MaxTokens: 100, Timeout: time.Second}, nil},
		{"temperature", llm.TierAssert, llm.TierConfig{Model: "m", Temperature: &temp}, []string{"assert tier", "claude-sbx", "temperature"}},
		{"base_url", llm.TierDistill, llm.TierConfig{Model: "m", BaseURL: "https://gateway.example"}, []string{"distill tier", "claude-sbx", "base_url"}},
		{"api_key_env", llm.TierDistill, llm.TierConfig{Model: "m", APIKeyEnv: "MY_KEY"}, []string{"distill tier", "claude-sbx", "api_key_env"}},
		{"embed", llm.TierEmbed, llm.TierConfig{Model: "m", Dimensions: 8}, []string{"embed tier", "claude-sbx", "no embedding model"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry(t, map[llm.Tier]llm.TierConfig{tt.tier: tt.tc})
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("NewRegistry() = %v, want a registry", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewRegistry() built a registry, want a refusal mentioning %v", tt.want)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("NewRegistry() = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestCompleterChecksSbxWhenTheRegistryIsBuilt(t *testing.T) {
	rig := claudesbxtest.Install(t, "")
	tiers := map[llm.Tier]llm.TierConfig{llm.TierDistill: {Model: "m"}, llm.TierAssert: {Model: "m"}}
	env := func(string) string { return "" }

	if _, err := registry(t, tiers, llm.WithEnv(env)); err != nil {
		t.Fatalf("NewRegistry() with sbx answering and no credential = %v, want a registry", err)
	}
	if got := len(rig.Lines(t, "sbx-ls.txt")); got != 2 {
		t.Errorf("sbx ls ran %d times, want once per tier", got)
	}
	if n := rig.Creates(t); n != 0 {
		t.Errorf("building the registry created %d sandboxes, want none", n)
	}

	rig.Mark(t, "fail-ls")
	_, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierAssert: {Model: "m"}}, llm.WithEnv(env))
	if err == nil {
		t.Fatal("NewRegistry() with sbx unavailable = nil, want a refusal")
	}
	for _, want := range []string{"assert tier", "claude-sbx", "sbx", "not available"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("NewRegistry() = %q, want it to mention %q", err, want)
		}
	}
}

func TestCompleteStructuredIsOneSandboxedCall(t *testing.T) {
	tmp := filepath.Dir(t.TempDir())
	t.Setenv("TMPDIR", tmp)
	rig := claudesbxtest.Install(t, fixture(t, "structured_answer.jsonl"))
	reg, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierDistill: {Model: "claude-test-model", MaxTokens: 1500}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Completer(llm.TierDistill)

	resp, err := c.Complete(t.Context(), request(summarySchema))
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if string(resp.JSON) != `{"summary":"Postgres was chosen","topics":["storage"]}` || resp.Text != "" {
		t.Errorf("answer = JSON %s, Text %q", resp.JSON, resp.Text)
	}
	if resp.StopReason != llm.StopEnd {
		t.Errorf("StopReason = %q, want %q", resp.StopReason, llm.StopEnd)
	}
	if rig.Runs(t) != 1 || rig.Creates(t) != 1 {
		t.Errorf("runs, creates = %d, %d, want one sandbox, created and run in", rig.Runs(t), rig.Creates(t))
	}
	for flag, want := range map[string]string{
		"--model":         "claude-test-model",
		"--system-prompt": "You distill team chat.",
		"--tools":         "",
		"--json-schema":   string(summarySchema.Definition),
	} {
		if got, ok := rig.Arg(t, flag); !ok || got != want {
			t.Errorf("claude %s = %q (present %v), want %q", flag, got, ok, want)
		}
	}
	if got := rig.Stdin(t); got != "Why Postgres?" {
		t.Errorf("prompt = %q, want the one message as it was", got)
	}
	pwd := strings.TrimSpace(strings.Join(rig.Lines(t, "claude-pwd.txt"), ""))
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	if !strings.HasPrefix(pwd, tmp+string(filepath.Separator)) {
		t.Errorf("claude ran in %q, want a temporary folder under %q", pwd, tmp)
	}
	if got := strings.TrimSpace(strings.Join(rig.Lines(t, "claude-files.txt"), "")); got != "0" {
		t.Errorf("the working folder held %s entries, want it empty", got)
	}
	if _, err := os.Stat(pwd); err == nil {
		t.Errorf("working folder %s was left behind", pwd)
	}
	if err := reg.Close(t.Context()); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if rig.Removes(t) != 1 {
		t.Errorf("removes after Close = %d, want the sandbox removed", rig.Removes(t))
	}
}

func TestCallsReuseTheTiersSandbox(t *testing.T) {
	rig := claudesbxtest.Install(t, fixture(t, "structured_answer.jsonl"))
	reg, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierDistill: {Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Completer(llm.TierDistill)
	for i := range 3 {
		if _, err := c.Complete(t.Context(), request(summarySchema)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if rig.Runs(t) != 3 || rig.Creates(t) != 1 || rig.Removes(t) != 0 {
		t.Errorf("runs, creates, removes = %d, %d, %d, want three calls in one sandbox kept for the next", rig.Runs(t), rig.Creates(t), rig.Removes(t))
	}

	if create := strings.Join(rig.Lines(t, "sbx-create.txt"), " "); !strings.Contains(create, "--memory "+claudesbx.SandboxMemory) {
		t.Errorf("sbx create %q, want --memory %s", create, claudesbx.SandboxMemory)
	}
	if err := reg.Close(t.Context()); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if rig.Removes(t) != 1 {
		t.Errorf("removes after Close = %d, want 1", rig.Removes(t))
	}
	if _, err := c.Complete(t.Context(), request(summarySchema)); err == nil {
		t.Error("a call after Close succeeded")
	}
}

func TestDockerHubsPullLimitIsARateLimit(t *testing.T) {
	rig := claudesbxtest.Install(t, fixture(t, "structured_answer.jsonl"))
	rig.Mark(t, "rate-limit-create")
	reg, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierDistill: {Model: "m", MaxRetries: -1}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Completer(llm.TierDistill)
	_, err = c.Complete(t.Context(), request(summarySchema))
	var e *llm.Error
	if !errors.As(err, &e) {
		t.Fatalf("Complete() = %v, want an *llm.Error", err)
	}
	if !e.Retryable || e.Kind != "registry_rate_limit" || e.RetryAfter != claudesbx.RegistryRateLimitWait {
		t.Errorf("error = retryable %v, kind %q, retry after %s; want a retryable registry_rate_limit waiting %s", e.Retryable, e.Kind, e.RetryAfter, claudesbx.RegistryRateLimitWait)
	}
	if rig.Runs(t) != 0 {
		t.Errorf("claude ran %d times with no sandbox", rig.Runs(t))
	}
}

func TestCompleteLabelsEveryTurnOfAConversation(t *testing.T) {
	rig := claudesbxtest.Install(t, fixture(t, "text_answer.jsonl"))
	c := distill(t, llm.TierConfig{})
	_, err := c.Complete(t.Context(), llm.Request{System: "sys", Messages: []llm.Message{
		{Role: llm.RoleUser, Text: "first question"},
		{Role: llm.RoleAssistant, Text: "first answer"},
		{Role: llm.RoleUser, Text: "second question"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rig.Runs(t) != 1 {
		t.Errorf("a conversation made %d calls, want 1", rig.Runs(t))
	}
	want := "User:\nfirst question\n\nAssistant:\nfirst answer\n\nUser:\nsecond question"
	if got := rig.Stdin(t); got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}

func TestCompleteTextIsTheFinalText(t *testing.T) {
	claudesbxtest.Install(t, fixture(t, "text_answer.jsonl"))
	c := distill(t, llm.TierConfig{})
	resp, err := c.Complete(t.Context(), request(nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "The team chose Postgres." || len(resp.JSON) != 0 || resp.StopReason != llm.StopEnd {
		t.Errorf("response = %+v, want the final text and the normal-end stop reason", resp)
	}
	if resp.Model != "claude-sonnet-5-5-20261001" {
		t.Errorf("Model = %q, want the model the call reports", resp.Model)
	}
	if resp.Tokens != (llm.Tokens{Input: 11, Output: 22}) {
		t.Errorf("Tokens = %+v", resp.Tokens)
	}
}

func TestCompleteRefusesWhatIsNotAnAnswer(t *testing.T) {
	for _, tt := range []struct {
		name    string
		stream  string
		schema  *llm.Schema
		wantErr string
		retry   bool
	}{
		{"a shape that breaks the schema", "structured_wrong_shape.jsonl", summarySchema, "does not satisfy summary", false},
		{"no structured output, as a success", "no_structured_output.jsonl", summarySchema, "without structured output", false},
		{"no structured output, retries exhausted", "structured_retries_exhausted.jsonl", summarySchema, "no_structured_output", false},
		{"a failed turn", "failed_turn.jsonl", nil, "the sandboxed call failed", false},
		{"no text", "empty_text.jsonl", nil, "no text", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claudesbxtest.Install(t, fixture(t, tt.stream))
			c := distill(t, llm.TierConfig{})
			resp, err := c.Complete(t.Context(), request(tt.schema))
			if err == nil {
				t.Fatalf("Complete() = %+v, want an error", resp)
			}
			if len(resp.JSON) != 0 || resp.Text != "" {
				t.Errorf("an error came with an answer: %+v", resp)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Complete() = %q, want it to mention %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), leak) {
				t.Errorf("Complete() = %q, which carries answer or result text", err)
			}
			if llm.Retryable(err) != tt.retry {
				t.Errorf("Retryable() = %v, want %v", llm.Retryable(err), tt.retry)
			}
			var e *llm.Error
			if tt.name != "a shape that breaks the schema" && !errors.As(err, &e) {
				t.Errorf("Complete() = %T, want an *llm.Error", err)
			}
		})
	}
}

func TestUsageCarriesTokensAndReportedCost(t *testing.T) {
	rig := claudesbxtest.Install(t, "")
	reg, err := registry(t, map[llm.Tier]llm.TierConfig{
		llm.TierDistill: {Model: "m"},
		llm.TierAssert:  {Model: "m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	distiller, _ := reg.Completer(llm.TierDistill)
	asserter, _ := reg.Completer(llm.TierAssert)

	rig.SetStream(t, fixture(t, "structured_answer.jsonl"))
	resp, err := distiller.Complete(t.Context(), request(summarySchema))
	if err != nil {
		t.Fatal(err)
	}
	ru := resp.ReportedUsage
	if ru == nil || ru.InputTokens == nil || *ru.InputTokens != 120 || ru.OutputTokens == nil || *ru.OutputTokens != 30 || ru.CostUSD == nil || *ru.CostUSD != 0.5 {
		t.Fatalf("ReportedUsage = %+v, want 120 in, 30 out and a cost of 0.5", ru)
	}
	rig.SetStream(t, fixture(t, "text_answer.jsonl"))
	if _, err := distiller.Complete(t.Context(), request(nil)); err != nil {
		t.Fatal(err)
	}
	rig.SetStream(t, fixture(t, "zero_cost.jsonl"))
	if _, err := asserter.Complete(t.Context(), request(nil)); err != nil {
		t.Fatal(err)
	}

	byKey := map[string]llm.Usage{}
	for _, u := range reg.Usage() {
		byKey[string(u.Tier)+"/"+u.Provider+"/"+u.Model] = u
	}
	want := byKey["distill/claude-sbx/claude-sonnet-5-5-20261001"]
	if want.Calls != 2 || want.InputTokens != 131 || want.OutputTokens != 52 {
		t.Errorf("distill usage = %+v, want 2 calls, 131 in and 52 out", want)
	}
	if !want.CostReported || want.CostUSD != 0.5+0.0123 {
		t.Errorf("distill cost = %v (reported %v), want 0.5123 as reported", want.CostUSD, want.CostReported)
	}
	zero := byKey["assert/claude-sbx/claude-sonnet-5-5-20261001"]
	if zero.Calls != 1 || !zero.CostReported || zero.CostUSD != 0 {
		t.Errorf("assert usage = %+v, want one call with a reported cost of zero", zero)
	}

	rig.SetStream(t, fixture(t, "no_cost.jsonl"))
	resp, err = asserter.Complete(t.Context(), request(nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ReportedUsage.CostUSD != nil {
		t.Errorf("a call that reported no cost has ReportedUsage cost %v", *resp.ReportedUsage.CostUSD)
	}
}

func TestUsageWithNoCostReportedShowsNone(t *testing.T) {
	claudesbxtest.Install(t, fixture(t, "no_cost.jsonl"))
	reg, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierDistill: {Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Completer(llm.TierDistill)
	if _, err := c.Complete(t.Context(), request(nil)); err != nil {
		t.Fatal(err)
	}
	u := reg.Usage()
	if len(u) != 1 || u[0].CostReported || u[0].CostUSD != 0 || u[0].Calls != 1 {
		t.Errorf("Usage() = %+v, want one call and no reported cost", u)
	}
}

func TestUsageLimitIsRetryableUntilTheReset(t *testing.T) {
	reset := time.Now().Add(90 * time.Minute).Truncate(time.Second)
	rig := claudesbxtest.Install(t, fixture(t, "usage_limit.jsonl", "@RESETS_AT@", strconv.FormatInt(reset.Unix(), 10)))
	reg, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierDistill: {Model: "m", MaxRetries: -1}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Completer(llm.TierDistill)
	_, err = c.Complete(t.Context(), request(summarySchema))
	var e *llm.Error
	if !errors.As(err, &e) {
		t.Fatalf("Complete() = %v, want an *llm.Error", err)
	}
	if !e.Retryable {
		t.Error("the usage limit is not retryable")
	}
	if e.RetryAfter < 89*time.Minute || e.RetryAfter > 90*time.Minute {
		t.Errorf("RetryAfter = %s, want the time until the reset, about 90m", e.RetryAfter)
	}
	if strings.Contains(err.Error(), leak) || strings.Contains(err.Error(), "11:50pm") {
		t.Errorf("Complete() = %q, which carries claude's own text", err)
	}
	if rig.Runs(t) != 1 {
		t.Errorf("a call with no retries ran claude %d times", rig.Runs(t))
	}
}

func TestACallersDeadlineEndsTheWaitForAUsageLimit(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	rig := claudesbxtest.Install(t, fixture(t, "usage_limit.jsonl", "@RESETS_AT@", strconv.FormatInt(reset.Unix(), 10)))
	reg, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierDistill: {Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Completer(llm.TierDistill)
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = c.Complete(ctx, request(nil))
	if took := time.Since(started); took > 20*time.Second {
		t.Errorf("Complete() took %s to give up on a one-hour wait with a 1.5s deadline", took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Complete() = %v, want the deadline", err)
	}
	if rig.Runs(t) != 1 {
		t.Errorf("claude ran %d times, want one: the retry was still waiting", rig.Runs(t))
	}
}

func TestMaxTokensIsTheOutputTokenBudget(t *testing.T) {
	rig := claudesbxtest.Install(t, fixture(t, "text_answer.jsonl"))
	t.Setenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS", "999")
	c := distill(t, llm.TierConfig{MaxTokens: 777})

	for _, tt := range []struct {
		name     string
		override int
		want     string
	}{
		{"the tier's", 0, "777"},
		{"the request's", 555, "555"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := request(nil)
			req.MaxTokens = tt.override
			if _, err := c.Complete(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if got, ok := rig.Env(t, "CLAUDE_CODE_MAX_OUTPUT_TOKENS"); !ok || got != tt.want {
				t.Errorf("CLAUDE_CODE_MAX_OUTPUT_TOKENS = %q (set %v), want %s", got, ok, tt.want)
			}
		})
	}
}

func TestConfiguredTimeoutWins(t *testing.T) {
	rig := claudesbxtest.Install(t, fixture(t, "text_answer.jsonl"))
	c := distill(t, llm.TierConfig{Timeout: 300 * time.Millisecond, MaxRetries: -1})
	rig.Mark(t, "hang-create")
	started := time.Now()
	_, err := c.Complete(t.Context(), request(nil))
	if err == nil {
		t.Fatal("Complete() with a sandbox that never starts = nil")
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("Complete() took %s, want the configured 300ms to end the attempt", took)
	}
	if !llm.Retryable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Complete() = %v, want a retryable timeout", err)
	}
	if rig.Runs(t) != 0 {
		t.Error("claude ran although the sandbox never started")
	}
}

func TestCompleteNeedsASystemPrompt(t *testing.T) {
	rig := claudesbxtest.Install(t, fixture(t, "text_answer.jsonl"))
	c := distill(t, llm.TierConfig{})
	_, err := c.Complete(t.Context(), llm.Request{Messages: user("hello")})
	if err == nil || !strings.Contains(err.Error(), "system prompt") || llm.Retryable(err) {
		t.Errorf("Complete() = %v, want a refusal that is not retried", err)
	}
	if rig.Creates(t) != 0 {
		t.Error("a sandbox was created for a request that cannot run")
	}
}

type spy struct {
	*claudesbx.Provider
	builds *[]llm.Build
}

func (s spy) Completer(b llm.Build) (llm.Completer, error) {
	*s.builds = append(*s.builds, b)
	return s.Provider.Completer(b)
}

func TestDefaultTimeoutIsLongerAndAConfiguredOneWins(t *testing.T) {
	claudesbxtest.Install(t, "")
	for _, tt := range []struct {
		name       string
		configured time.Duration
		check      func(time.Duration) bool
		want       string
	}{
		{"none configured", 0, func(d time.Duration) bool { return d > 60*time.Second && d < 2*time.Minute }, "more than 60s and under 2m"},
		{"configured", 7 * time.Second, func(d time.Duration) bool { return d == 7*time.Second }, "7s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var builds []llm.Build
			cfg := llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
				llm.TierDistill: {Provider: llm.ProviderClaudeSbx, Model: "m", Timeout: tt.configured},
			}}
			if _, err := llm.NewRegistry(cfg, []llm.Provider{spy{claudesbx.New(), &builds}}); err != nil {
				t.Fatalf("NewRegistry() = %v", err)
			}
			if got := builds[0].Config.Timeout; !tt.check(got) {
				t.Errorf("the tier was built with a timeout of %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNoCredentialReachesClaude(t *testing.T) {
	rig := claudesbxtest.Install(t, fixture(t, "text_answer.jsonl"))
	c := distill(t, llm.TierConfig{})
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-travel")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-should-not-travel")
	if _, err := c.Complete(t.Context(), request(nil)); err != nil {
		t.Fatal(err)
	}
	for _, kv := range rig.Lines(t, "claude-env.txt") {
		if strings.Contains(kv, "should-not-travel") {
			t.Errorf("claude's environment holds %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}
