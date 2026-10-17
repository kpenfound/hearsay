package llm

import (
	"context"
	"math"
	"sync"
)

// CompletionAccounting collects one stage's calls and all provider attempts,
// including failed retries. It is scoped by context, never by process counters.
// Unknown or overflowing consumption stays unknown.
type CompletionAccounting struct {
	mu              sync.Mutex
	calls, attempts *int64
	usage           JudgmentUsage
}

type completionAccountingKey struct{}

// ObserveCompletions starts independent accounting for a worker stage.
func ObserveCompletions(ctx context.Context) (context.Context, *CompletionAccounting) {
	a := &CompletionAccounting{calls: int64ptr(0), attempts: int64ptr(0), usage: JudgmentUsage{InputTokens: int64ptr(0), OutputTokens: int64ptr(0), CostUSD: new(float64)}}
	return context.WithValue(ctx, completionAccountingKey{}, a), a
}

// Snapshot returns logical Complete calls, provider attempts and reported usage.
func (a *CompletionAccounting) Snapshot() (*int64, *int64, JudgmentUsage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls, a.attempts, a.usage
}

func addCompletionCount(a, b *int64) *int64 {
	if a == nil || b == nil || *a < 0 || *b < 0 || *a > math.MaxInt64-*b {
		return nil
	}
	return int64ptr(*a + *b)
}

func int64ptr(n int64) *int64 { return &n }

func completionUsage(resp Response) JudgmentUsage {
	if resp.ReportedUsage != nil {
		u := *resp.ReportedUsage
		SanitizeJudgmentUsage(&u)
		return u
	}
	// Legacy providers have no presence bits: positive counts are reported, zero
	// cannot be distinguished from absence. Never infer provider cost from price.
	var u JudgmentUsage
	if resp.Tokens.Input > 0 {
		u.InputTokens = int64ptr(int64(resp.Tokens.Input))
	}
	if resp.Tokens.Output > 0 {
		u.OutputTokens = int64ptr(int64(resp.Tokens.Output))
	}
	return u
}

func recordCompletionAttempt(ctx context.Context, resp Response) {
	if a, ok := ctx.Value(completionAccountingKey{}).(*CompletionAccounting); ok {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.attempts = addCompletionCount(a.attempts, int64ptr(1))
		a.usage = AddJudgmentUsage(a.usage, completionUsage(resp))
	}
}

// AccountCompletions preserves the Completer interface. Registry attempts are
// observed inside its retry loop. A custom Completer without that instrumentation
// still counts as a call, but its attempt count remains unknown.
func AccountCompletions(c Completer) Completer { return accountedCompleter{c} }

type accountedCompleter struct{ inner Completer }

func (c accountedCompleter) Complete(ctx context.Context, req Request) (Response, error) {
	parent, _ := ctx.Value(completionAccountingKey{}).(*CompletionAccounting)
	if parent == nil {
		return c.inner.Complete(ctx, req)
	}
	callCtx, local := ObserveCompletions(ctx)
	resp, err := c.inner.Complete(callCtx, req)
	_, attempts, usage := local.Snapshot()
	if attempts != nil && *attempts == 0 {
		attempts, usage = nil, completionUsage(resp)
	}
	parent.mu.Lock()
	parent.calls = addCompletionCount(parent.calls, int64ptr(1))
	parent.attempts = addCompletionCount(parent.attempts, attempts)
	parent.usage = AddJudgmentUsage(parent.usage, usage)
	parent.mu.Unlock()
	return resp, err
}
