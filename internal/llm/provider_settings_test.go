package llm_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
)

func TestNewRegistryRefusesSettingsAProviderHasNoUseFor(t *testing.T) {
	temp := 0.2
	for _, tt := range []struct {
		name    string
		tc      llm.TierConfig
		wantErr []string
	}{
		{"nothing set", llm.TierConfig{Provider: "bare", Model: "m"}, nil},
		{"a budget and a timeout are not refused", llm.TierConfig{Provider: "bare", Model: "m", MaxTokens: 100, Timeout: time.Second}, nil},
		{"temperature", llm.TierConfig{Provider: "bare", Model: "m", Temperature: &temp}, []string{"distill tier", "temperature"}},
		{"base_url", llm.TierConfig{Provider: "bare", Model: "m", BaseURL: "https://gateway.example"}, []string{"distill tier", "base_url"}},
		{"api_key_env", llm.TierConfig{Provider: "bare", Model: "m", APIKeyEnv: "MY_KEY"}, []string{"distill tier", "api_key_env"}},
		{"every one is reported", llm.TierConfig{Provider: "bare", Model: "m", Temperature: &temp, BaseURL: "https://g.example", APIKeyEnv: "K"},
			[]string{"temperature", "base_url", "api_key_env"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newStub("bare")
			p.caps.NoTemperature, p.caps.NoBaseURL, p.caps.NoAPIKeyEnv = true, true, true
			_, err := llm.NewRegistry(cfg(llm.TierDistill, tt.tc), []llm.Provider{p})
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("NewRegistry() = %v, want a registry", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewRegistry() built a registry, want a refusal naming %v", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("NewRegistry() = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestNewRegistryTakesSettingsByDefault(t *testing.T) {
	temp := 0.2
	tc := llm.TierConfig{Provider: "anthropic", Model: "m", Temperature: &temp, BaseURL: "https://g.example", APIKeyEnv: "K"}
	if _, err := llm.NewRegistry(cfg(llm.TierDistill, tc), []llm.Provider{newStub("anthropic")}); err != nil {
		t.Fatalf("NewRegistry() = %v, want a registry", err)
	}
}

var temperatureRefusingCaps = llm.Capabilities{Complete: true, SystemPrompt: true, StructuredOutput: true, NoTemperature: true}

func TestCanServeAcceptsATierThatSetsNothingTheProviderRefuses(t *testing.T) {
	if err := llm.CanServe("x", temperatureRefusingCaps, llm.TierDistill, llm.TierConfig{Provider: "x", Model: "m"}); err != nil {
		t.Errorf("CanServe() = %v, want nil", err)
	}
}

func TestCanServeRefusesSettingsWhereConfigValidateAsks(t *testing.T) {
	temp := 0.2
	caps := temperatureRefusingCaps
	err := llm.CanServe("x", caps, llm.TierDistill, llm.TierConfig{Provider: "x", Model: "m", Temperature: &temp})
	if err == nil || !strings.Contains(err.Error(), "temperature") {
		t.Errorf("CanServe() = %v, want a refusal of temperature", err)
	}
}

func TestProviderDefaultTimeout(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider time.Duration
		timeout  time.Duration
		want     time.Duration
	}{
		{"no provider default", 0, 0, llm.DefaultTimeout},
		{"the provider's default", 100 * time.Second, 0, 100 * time.Second},
		{"configured wins", 100 * time.Second, 5 * time.Second, 5 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var builds []llm.Build
			p := newStub("slow")
			p.builds = &builds
			p.caps.DefaultTimeout = tt.provider
			tc := llm.TierConfig{Provider: "slow", Model: "m", Timeout: tt.timeout}
			if _, err := llm.NewRegistry(cfg(llm.TierDistill, tc), []llm.Provider{p}); err != nil {
				t.Fatalf("NewRegistry() = %v", err)
			}
			if got := builds[0].Config.Timeout; got != tt.want {
				t.Errorf("the tier was built with a timeout of %s, want %s", got, tt.want)
			}
		})
	}
}

func costOf(c float64) *llm.JudgmentUsage { return &llm.JudgmentUsage{CostUSD: &c} }

func TestUsageSumsReportedCost(t *testing.T) {
	answers := []llm.Response{
		{Text: "ok", Model: "m", Tokens: llm.Tokens{Input: 10, Output: 2}, ReportedUsage: costOf(0.25)},
		{Text: "ok", Model: "m", Tokens: llm.Tokens{Input: 5, Output: 1}, ReportedUsage: costOf(0.5)},

		{Text: "ok", Model: "m", ReportedUsage: costOf(math.NaN())},
		{Text: "ok", Model: "m", ReportedUsage: costOf(-1)},
		{Text: "ok", Model: "zero", ReportedUsage: costOf(0)},
		{Text: "ok", Model: "none", Tokens: llm.Tokens{Input: 3}},
		{Text: "ok", Model: "none", Tokens: llm.Tokens{Input: 4}, ReportedUsage: &llm.JudgmentUsage{}},
	}
	var next int
	reporting := newStub("reporting")
	reporting.complete = func(context.Context, llm.Request) (llm.Response, error) {
		next++
		return answers[next-1], nil
	}
	registry, err := llm.NewRegistry(cfg(llm.TierDistill, llm.TierConfig{Provider: "reporting", Model: "m"}), []llm.Provider{reporting})
	if err != nil {
		t.Fatal(err)
	}
	completer, _ := registry.Completer(llm.TierDistill)
	for range answers {
		if _, err := completer.Complete(t.Context(), ask("q")); err != nil {
			t.Fatalf("Complete() = %v", err)
		}
	}
	type row struct {
		calls        int
		in, out      int64
		cost         float64
		costReported bool
	}
	want := map[string]row{
		"m":    {calls: 4, in: 15, out: 3, cost: 0.75, costReported: true},
		"zero": {calls: 1, cost: 0, costReported: true},
		"none": {calls: 2, in: 7},
	}
	got := map[string]row{}
	for _, u := range registry.Usage() {
		got[u.Model] = row{u.Calls, u.InputTokens, u.OutputTokens, u.CostUSD, u.CostReported}
	}
	for model, w := range want {
		if got[model] != w {
			t.Errorf("Usage() for %s = %+v, want %+v", model, got[model], w)
		}
	}
}
