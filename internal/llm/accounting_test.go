package llm_test

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
)

func TestCompletionAccountingRetriesAndPresence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reported bool
	}{{"reported zero", true}, {"absent", false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newStub("accounting")
			var attempts atomic.Int64
			p.complete = func(context.Context, llm.Request) (llm.Response, error) {
				r := llm.Response{Text: "fixture", StopReason: llm.StopEnd}
				if tc.reported {
					n, cost := int64(0), float64(0)
					r.ReportedUsage = &llm.JudgmentUsage{InputTokens: &n, OutputTokens: &n, CostUSD: &cost}
				}
				if attempts.Add(1) == 1 {
					return r, &llm.Error{Retryable: true}
				}
				return r, nil
			}
			registry, err := llm.NewRegistry(llm.Config{Tiers: map[llm.Tier]llm.TierConfig{llm.TierDistill: {Provider: p.Name(), Model: "fixture", MaxRetries: 1, Backoff: time.Nanosecond}}}, []llm.Provider{p})
			if err != nil {
				t.Fatal(err)
			}
			c, err := registry.Completer(llm.TierDistill)
			if err != nil {
				t.Fatal(err)
			}
			ctx, a := llm.ObserveCompletions(t.Context())
			if _, err := llm.AccountCompletions(c).Complete(ctx, ask("retry")); err != nil {
				t.Fatal(err)
			}
			calls, tries, u := a.Snapshot()
			if calls == nil || *calls != 1 || tries == nil || *tries != 2 {
				t.Fatalf("calls=%v attempts=%v", calls, tries)
			}
			if tc.reported {
				if u.InputTokens == nil || *u.InputTokens != 0 || u.OutputTokens == nil || *u.OutputTokens != 0 || u.CostUSD == nil || *u.CostUSD != 0 {
					t.Fatalf("zero lost: %+v", u)
				}
			} else if u.InputTokens != nil || u.OutputTokens != nil || u.CostUSD != nil {
				t.Fatalf("manufactured usage: %+v", u)
			}
			if got := registry.Usage(); len(got) != 1 {
				t.Fatalf("process accounting lost: %+v", got)
			}
		})
	}
}

func TestCompletionAccountingConcurrentStages(t *testing.T) {
	p := newStub("concurrent")
	p.complete = func(_ context.Context, r llm.Request) (llm.Response, error) {
		n := int64(len(r.Messages[0].Text))
		return llm.Response{Text: "fixture", ReportedUsage: &llm.JudgmentUsage{InputTokens: &n}}, nil
	}
	r, err := llm.NewRegistry(llm.Config{Tiers: map[llm.Tier]llm.TierConfig{llm.TierAssert: {Provider: p.Name(), Model: "fixture"}}}, []llm.Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Completer(llm.TierAssert)
	if err != nil {
		t.Fatal(err)
	}
	c = llm.AccountCompletions(c)
	var wg sync.WaitGroup
	for _, text := range []string{"a", "a longer input"} {
		wg.Go(func() {
			ctx, a := llm.ObserveCompletions(t.Context())
			for range 20 {
				if _, err := c.Complete(ctx, ask(text)); err != nil {
					t.Error(err)
				}
			}
			calls, attempts, u := a.Snapshot()
			if calls == nil || *calls != 20 || attempts == nil || *attempts != 20 || u.InputTokens == nil || *u.InputTokens != int64(20*len(text)) {
				t.Errorf("mixed stage accounting: %v %v %+v", calls, attempts, u)
			}
		})
	}
	wg.Wait()
}

func TestCompletionAccountingIncludesFailedAttemptConsumption(t *testing.T) {
	p := newStub("billed-retry")
	var n atomic.Int64
	p.complete = func(context.Context, llm.Request) (llm.Response, error) {
		attempt := n.Add(1)
		tokens, cost := attempt*5, float64(attempt)*.1
		response := llm.Response{Text: "fixture", ReportedUsage: &llm.JudgmentUsage{InputTokens: &tokens, OutputTokens: &attempt, CostUSD: &cost}}
		if attempt == 1 {
			return response, &llm.Error{Retryable: true}
		}
		return response, nil
	}
	r, err := llm.NewRegistry(llm.Config{Tiers: map[llm.Tier]llm.TierConfig{llm.TierDistill: {Provider: p.Name(), Model: "fixture", MaxRetries: 1, Backoff: time.Nanosecond}}}, []llm.Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.Completer(llm.TierDistill)
	if err != nil {
		t.Fatal(err)
	}
	ctx, a := llm.ObserveCompletions(t.Context())
	if _, err = llm.AccountCompletions(c).Complete(ctx, ask("billed retry")); err != nil {
		t.Fatal(err)
	}
	_, attempts, u := a.Snapshot()
	if attempts == nil || *attempts != 2 || u.InputTokens == nil || *u.InputTokens != 15 || u.OutputTokens == nil || *u.OutputTokens != 3 || u.CostUSD == nil || math.Abs(*u.CostUSD-.3) > 1e-12 {
		t.Fatalf("retry consumption omitted: attempts=%v usage=%+v", attempts, u)
	}
}
