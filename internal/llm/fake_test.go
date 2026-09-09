package llm_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"testing/fstest"

	"github.com/kpenfound/hearsay/internal/llm"
)

// fixtureConfig is the configuration the checked-in fixtures were recorded
// against: the shipped defaults, plus the embed tier they exercise.
func fixtureConfig() llm.Config {
	config := llm.Default().Clone()
	config.Tiers[llm.TierEmbed] = llm.TierConfig{Provider: "voyage", Model: "voyage-3", Dimensions: 8}
	return config
}

func loadFixtures(t *testing.T) *llm.Fixtures {
	t.Helper()
	fixtures, err := llm.LoadFixtures(os.DirFS("testdata/fixtures"))
	if err != nil {
		t.Fatalf("LoadFixtures(testdata/fixtures) = %v", err)
	}
	return fixtures
}

// The distill and assert tiers answer from what was recorded, and the answer to
// a structured request is JSON satisfying the schema that was asked for.
func TestFakeReplaysCompletions(t *testing.T) {
	registry, err := llm.NewFake(fixtureConfig(), loadFixtures(t))
	if err != nil {
		t.Fatalf("NewFake() = %v", err)
	}

	distill, err := registry.Completer(llm.TierDistill)
	if err != nil {
		t.Fatalf("Completer(distill) = %v", err)
	}
	req := llm.Request{
		System:   "You distil one artifact into one document.",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "PR #62: the job queue moves to Postgres. Reviewed by robin, merged on Friday."}},
		Schema:   schema(distillSchema),
	}
	resp, err := distill.Complete(t.Context(), req)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	var answer struct {
		Summary     string `json:"summary"`
		OutcomeKind string `json:"outcome_kind"`
	}
	if err := json.Unmarshal(resp.JSON, &answer); err != nil {
		t.Fatalf("the recorded answer is not JSON: %v", err)
	}
	if answer.OutcomeKind != "decided" {
		t.Errorf("outcome_kind = %q, want decided", answer.OutcomeKind)
	}
	if resp.Tokens != (llm.Tokens{Input: 412, Output: 96}) {
		t.Errorf("Tokens = %+v, want what the recording says it cost", resp.Tokens)
	}

	assert, err := registry.Completer(llm.TierAssert)
	if err != nil {
		t.Fatalf("Completer(assert) = %v", err)
	}
	resp, err = assert.Complete(t.Context(), llm.Request{
		System:   "You extract one stance and the topic it is about.",
		Messages: []llm.Message{{Role: llm.RoleUser, Text: "The job queue moves to Postgres; robin reviewed and it merged on Friday."}},
	})
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if resp.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the model the recording was made with", resp.Model)
	}

	// The two tiers have different budgets, and both were spent through one
	// registry, so the accounting has a row each.
	usage := registry.Usage()
	if len(usage) != 2 {
		t.Fatalf("Usage() = %+v, want one row per tier", usage)
	}
	if usage[0].Tier != llm.TierDistill || usage[0].InputTokens != 412 || usage[1].Tier != llm.TierAssert {
		t.Errorf("Usage() = %+v, want the distill row first with what it cost", usage)
	}
}

// One vector per input, in order, of the width the tier is configured for.
func TestFakeReplaysEmbeddings(t *testing.T) {
	registry, err := llm.NewFake(fixtureConfig(), loadFixtures(t))
	if err != nil {
		t.Fatalf("NewFake() = %v", err)
	}
	embedder, err := registry.Embedder()
	if err != nil {
		t.Fatalf("Embedder() = %v", err)
	}
	if got := embedder.Dimensions(); got != 8 {
		t.Errorf("Dimensions() = %d, want the 8 the fixture holds", got)
	}
	texts := []string{"The job queue moves to Postgres.", "The distiller calls the cheap tier once per artifact."}
	vectors, err := embedder.Embed(t.Context(), texts)
	if err != nil {
		t.Fatalf("Embed() = %v", err)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("Embed() returned %d vectors for %d texts", len(vectors), len(texts))
	}
	if vectors[0][0] != 0.011 || vectors[1][0] != -0.24 {
		t.Errorf("Embed() = %v, want the recorded vectors in the order the texts were given", vectors)
	}
	// The same texts the other way round are a different call, and nothing
	// recorded one.
	if _, err := embedder.Embed(t.Context(), []string{texts[1], texts[0]}); !errors.Is(err, llm.ErrNoFixture) {
		t.Errorf("Embed(the texts reversed) = %v, want ErrNoFixture", err)
	}

	// A tier whose configured width is not the recording's is refused rather
	// than written into a column that does not fit it.
	config := fixtureConfig()
	config.Tiers[llm.TierEmbed] = llm.TierConfig{Provider: "voyage", Model: "voyage-3", Dimensions: 1024}
	wrong, err := llm.NewFake(config, loadFixtures(t))
	if err != nil {
		t.Fatalf("NewFake() = %v", err)
	}
	embedder, _ = wrong.Embedder()
	if _, err := embedder.Embed(t.Context(), texts); !errors.Is(err, llm.ErrDimensions) {
		t.Errorf("Embed() with the wrong width configured = %v, want ErrDimensions", err)
	}
}

// A request nothing recorded is the error a test that would otherwise reach a
// provider gets, and it says how to fix it without printing the prompt.
func TestFakeRefusesWhatWasNotRecorded(t *testing.T) {
	registry, err := llm.NewFake(fixtureConfig(), loadFixtures(t))
	if err != nil {
		t.Fatalf("NewFake() = %v", err)
	}
	completer, _ := registry.Completer(llm.TierDistill)
	secret := "the payload nobody may log"
	_, err = completer.Complete(t.Context(), ask(secret))
	if !errors.Is(err, llm.ErrNoFixture) {
		t.Fatalf("Complete() = %v, want ErrNoFixture", err)
	}
	if contains(err.Error(), secret) {
		t.Errorf("the miss quotes the prompt: %q", err)
	}
	if !contains(err.Error(), "sha256:") {
		t.Errorf("the miss = %q, want it to name the key to record", err)
	}
}

// The key is the request, canonicalised: the same call is the same key however
// the schema was written out, and any difference that changes the answer is a
// different key.
func TestFixtureKey(t *testing.T) {
	base := llm.Request{
		System:    "s",
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: "a"}},
		MaxTokens: 100,
		Schema:    &llm.Schema{Name: "x", Definition: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)},
	}
	reordered := base
	reordered.Schema = &llm.Schema{Name: "x", Definition: json.RawMessage("{\n  \"properties\": {\"a\": {\"type\": \"string\"}},\n  \"type\": \"object\"\n}")}
	if llm.FixtureKey(llm.TierDistill, base) != llm.FixtureKey(llm.TierDistill, reordered) {
		t.Error("the same schema written differently keys differently")
	}

	for _, tt := range []struct {
		name   string
		change func(*llm.Request)
	}{
		{name: "the system prompt", change: func(r *llm.Request) { r.System = "t" }},
		{name: "the message", change: func(r *llm.Request) { r.Messages = []llm.Message{{Role: llm.RoleUser, Text: "b"}} }},
		{name: "the budget", change: func(r *llm.Request) { r.MaxTokens = 200 }},
		{name: "the schema", change: func(r *llm.Request) {
			r.Schema = &llm.Schema{Name: "x", Definition: json.RawMessage(`{"type":"object"}`)}
		}},
		{name: "asking for no schema at all", change: func(r *llm.Request) { r.Schema = nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			tt.change(&changed)
			if llm.FixtureKey(llm.TierDistill, changed) == llm.FixtureKey(llm.TierDistill, base) {
				t.Errorf("changing %s does not change the key", tt.name)
			}
		})
	}
	// The tier is part of the key: the same prompt to two tiers is two calls.
	if llm.FixtureKey(llm.TierDistill, base) == llm.FixtureKey(llm.TierAssert, base) {
		t.Error("the same request keys the same on two tiers")
	}
}

// A fixture that does not describe something this abstraction could have done
// is refused when it is loaded, rather than replaying something impossible.
func TestLoadFixturesRefusesMalformedRecordings(t *testing.T) {
	for _, tt := range []struct {
		name    string
		file    string
		wantErr string
	}{
		{
			name:    "an answer that does not satisfy the schema it was asked for",
			file:    `{"completions":[{"tier":"distill","request":{"messages":[{"role":"user","text":"a"}],"schema":{"name":"s","definition":{"type":"object","required":["k"]}}},"response":{"json":{}}}]}`,
			wantErr: `the field "k" is missing`,
		},
		{
			name:    "a structured request with no JSON recorded",
			file:    `{"completions":[{"tier":"distill","request":{"messages":[{"role":"user","text":"a"}],"schema":{"name":"s","definition":{"type":"object"}}},"response":{"text":"here you go"}}]}`,
			wantErr: "no json",
		},
		{
			name:    "an empty answer",
			file:    `{"completions":[{"tier":"distill","request":{"messages":[{"role":"user","text":"a"}]},"response":{}}]}`,
			wantErr: "empty",
		},
		{
			name:    "a request no provider would take",
			file:    `{"completions":[{"tier":"distill","request":{"messages":[{"role":"assistant","text":"a"}]},"response":{"text":"x"}}]}`,
			wantErr: "not one this package would send",
		},
		{
			name:    "a completion recorded against the embed tier",
			file:    `{"completions":[{"tier":"embed","request":{"messages":[{"role":"user","text":"a"}]},"response":{"text":"x"}}]}`,
			wantErr: "a completion is recorded against",
		},
		{
			name:    "two recordings of the same request",
			file:    `{"completions":[{"tier":"distill","request":{"messages":[{"role":"user","text":"a"}]},"response":{"text":"x"}},{"tier":"distill","request":{"messages":[{"role":"user","text":"a"}]},"response":{"text":"y"}}]}`,
			wantErr: "already recorded",
		},
		{
			name:    "vectors that are not one per text",
			file:    `{"embeddings":[{"texts":["a","b"],"vectors":[[0.1]]}]}`,
			wantErr: "1 vectors for 2 texts",
		},
		{
			name:    "vectors of two widths",
			file:    `{"embeddings":[{"texts":["a","b"],"vectors":[[0.1],[0.2,0.3]]}]}`,
			wantErr: "one width per model",
		},
		{
			name:    "a key that is not a field",
			file:    `{"completions":[],"embedings":[]}`,
			wantErr: "embedings",
		},
		{
			name:    "not JSON",
			file:    `completions: []`,
			wantErr: "invalid character",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := llm.LoadFixtures(fstest.MapFS{"recorded.json": &fstest.MapFile{Data: []byte(tt.file)}})
			if err == nil {
				t.Fatalf("LoadFixtures() = nil, want an error about %q", tt.wantErr)
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("LoadFixtures() = %q, want it to mention %q", err, tt.wantErr)
			}
			if !contains(err.Error(), "recorded.json") {
				t.Errorf("LoadFixtures() = %q, want it to name the file", err)
			}
		})
	}
}

// Fixtures can also be built in a test, for a case that is about the shape of
// an answer rather than about a real recording.
func TestFixturesAddedByHand(t *testing.T) {
	fixtures := llm.NewFixtures()
	req := ask("what did they decide?")
	if err := fixtures.Add(llm.CompletionFixture{
		Tier:     llm.TierDistill,
		Request:  req,
		Response: llm.Response{Text: "they decided to ship", Tokens: llm.Tokens{Input: 5, Output: 4}},
	}); err != nil {
		t.Fatalf("Add() = %v", err)
	}
	if got := fixtures.Len(); got != 1 {
		t.Errorf("Len() = %d, want 1", got)
	}
	// The recording is keyed by the request as the tier will send it, budget
	// and all, so it has to be built with the same budget.
	config := cfg(llm.TierDistill, llm.TierConfig{Provider: "anthropic", Model: "small"})
	registry, err := llm.NewFake(config, fixtures)
	if err != nil {
		t.Fatalf("NewFake() = %v", err)
	}
	completer, _ := registry.Completer(llm.TierDistill)
	if _, err := completer.Complete(t.Context(), req); !errors.Is(err, llm.ErrNoFixture) {
		t.Fatalf("Complete() = %v, want the miss: the tier's budget is part of the key", err)
	}
	withBudget := req
	withBudget.MaxTokens = llm.DefaultMaxTokens
	if err := fixtures.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: withBudget,
		Response: llm.Response{Text: "they decided to ship"}}); err != nil {
		t.Fatalf("Add() = %v", err)
	}
	resp, err := completer.Complete(t.Context(), req)
	if err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	if resp.Text != "they decided to ship" {
		t.Errorf("Complete() = %q", resp.Text)
	}
	if resp.StopReason != llm.StopEnd {
		t.Errorf("StopReason = %q, want a recording with no stop reason to read as a complete answer", resp.StopReason)
	}
}
