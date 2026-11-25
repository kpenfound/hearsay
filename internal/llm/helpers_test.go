package llm_test

import (
	"context"

	"github.com/kpenfound/hearsay/internal/llm"
)

type stubProvider struct {
	name      string
	caps      llm.Capabilities
	complete  func(ctx context.Context, req llm.Request) (llm.Response, error)
	embed     func(ctx context.Context, texts []string) ([][]float32, llm.Tokens, error)
	dimension int

	builds *[]llm.Build

	failCompleter error
	failEmbedder  error
}

func newStub(name string) *stubProvider {
	return &stubProvider{
		name:      name,
		caps:      llm.Capabilities{Complete: true, Embed: true, SystemPrompt: true, StructuredOutput: true},
		dimension: 4,
	}
}

func (p *stubProvider) Name() string                   { return p.name }
func (p *stubProvider) Capabilities() llm.Capabilities { return p.caps }

func (p *stubProvider) Completer(b llm.Build) (llm.Completer, error) {
	if p.builds != nil {
		*p.builds = append(*p.builds, b)
	}
	if p.failCompleter != nil {
		return nil, p.failCompleter
	}
	return completerFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		if p.complete == nil {
			return llm.Response{Text: "ok", StopReason: llm.StopEnd, Model: b.Config.Model}, nil
		}
		return p.complete(ctx, req)
	}), nil
}

func (p *stubProvider) Embedder(b llm.Build) (llm.Embedder, error) {
	if p.builds != nil {
		*p.builds = append(*p.builds, b)
	}
	if p.failEmbedder != nil {
		return nil, p.failEmbedder
	}
	dimensions := b.Config.Dimensions
	if dimensions == 0 {
		dimensions = p.dimension
	}
	return &stubEmbedder{dimensions: dimensions, embed: p.embed}, nil
}

type completerFunc func(ctx context.Context, req llm.Request) (llm.Response, error)

func (f completerFunc) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	return f(ctx, req)
}

type stubEmbedder struct {
	dimensions int
	embed      func(ctx context.Context, texts []string) ([][]float32, llm.Tokens, error)
}

func (e *stubEmbedder) Dimensions() int { return e.dimensions }

func (e *stubEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vectors, _, err := e.EmbedTokens(ctx, texts)
	return vectors, err
}

func (e *stubEmbedder) EmbedTokens(ctx context.Context, texts []string) ([][]float32, llm.Tokens, error) {
	if e.embed != nil {
		return e.embed(ctx, texts)
	}
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = make([]float32, e.dimensions)
		vectors[i][0] = float32(i)
	}
	return vectors, llm.Tokens{Input: len(texts)}, nil
}

func fixedEmbedder(dimensions int) llm.Embedder { return &stubEmbedder{dimensions: dimensions} }

func ask(text string) llm.Request {
	return llm.Request{System: "you are a test", Messages: []llm.Message{{Role: llm.RoleUser, Text: text}}}
}
