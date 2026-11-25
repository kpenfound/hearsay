package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// FixtureFile is what a recorded fixture file holds: JSON, one object with a
// list of completions and a list of embeddings.
type FixtureFile struct {
	Completions []CompletionFixture `json:"completions,omitempty"`
	Embeddings  []EmbeddingFixture  `json:"embeddings,omitempty"`
}

// CompletionFixture is one recorded completion: which tier was asked, what it
// was asked, and what came back.
type CompletionFixture struct {
	Tier     Tier     `json:"tier"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

// EmbeddingFixture is one recorded embedding call. Vectors are in the order the
// texts are, which is the promise [Embedder.Embed] makes.
type EmbeddingFixture struct {
	Texts   []string    `json:"texts"`
	Vectors [][]float32 `json:"vectors"`
	// Tokens is what the recorded call was billed for, which the fake reports
	// the way a provider that counts them would.
	Tokens Tokens `json:"tokens,omitzero"`
}

// Fixtures is a set of recorded exchanges, keyed by request.
type Fixtures struct {
	completions map[string]CompletionFixture
	embeddings  map[string]EmbeddingFixture

	files map[string]string
}

// NewFixtures is an empty set, to add recordings to by hand.
func NewFixtures() *Fixtures {
	return &Fixtures{
		completions: map[string]CompletionFixture{},
		embeddings:  map[string]EmbeddingFixture{},
		files:       map[string]string{},
	}
}

// LoadFixtures reads every `.json` file under fsys, in path order, and checks
// each recording as it goes: a request that is not one this abstraction would
// send, an answer that does not satisfy the schema its request asked for, and
// two recordings of the same request are all errors.
func LoadFixtures(fsys fs.FS) (*Fixtures, error) {
	f := NewFixtures()
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path.Base(p), ".json") {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("reading fixture %s: %w", p, err)
		}
		var file FixtureFile
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&file); err != nil {
			return fmt.Errorf("fixture %s: %w", p, err)
		}
		for i, c := range file.Completions {
			if err := f.add(p, c); err != nil {
				return fmt.Errorf("fixture %s: completions[%d]: %w", p, i, err)
			}
		}
		for i, e := range file.Embeddings {
			if err := f.addEmbedding(p, e); err != nil {
				return fmt.Errorf("fixture %s: embeddings[%d]: %w", p, i, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Add records one completion.
func (f *Fixtures) Add(c CompletionFixture) error { return f.add("", c) }

// AddEmbedding records one embedding call.
func (f *Fixtures) AddEmbedding(e EmbeddingFixture) error { return f.addEmbedding("", e) }

// Len is how many recordings there are.
func (f *Fixtures) Len() int { return len(f.completions) + len(f.embeddings) }

func (f *Fixtures) add(file string, c CompletionFixture) error {
	if !c.Tier.Completion() {
		return fmt.Errorf("tier %q: a completion is recorded against %s or %s", c.Tier, TierDistill, TierAssert)
	}
	if err := c.Request.Validate(); err != nil {
		return fmt.Errorf("the recorded request is not one this package would send: %w", err)
	}
	switch {
	case c.Request.Schema != nil:
		if len(c.Response.JSON) == 0 {
			return fmt.Errorf("the request asks for %s and the recorded answer has no json", c.Request.Schema.Name)
		}
		if err := c.Request.Schema.ValidateJSON(c.Response.JSON); err != nil {
			return fmt.Errorf("the recorded answer does not satisfy %s: %w", c.Request.Schema.Name, err)
		}
	case c.Response.Text == "":
		return fmt.Errorf("the recorded answer is empty")
	}
	if c.Response.StopReason == "" {
		c.Response.StopReason = StopEnd
	}
	key := FixtureKey(c.Tier, c.Request)
	if err := f.claim(key, file); err != nil {
		return err
	}
	f.completions[key] = c
	return nil
}

func (f *Fixtures) addEmbedding(file string, e EmbeddingFixture) error {
	if len(e.Texts) == 0 {
		return fmt.Errorf("no texts")
	}
	if len(e.Vectors) != len(e.Texts) {
		return fmt.Errorf("%d vectors for %d texts: one vector per input, in order", len(e.Vectors), len(e.Texts))
	}
	for i, v := range e.Vectors {
		if len(v) == 0 {
			return fmt.Errorf("vectors[%d] is empty", i)
		}
		if len(v) != len(e.Vectors[0]) {
			return fmt.Errorf("vectors[%d] has %d values and vectors[0] has %d: one width per model", i, len(v), len(e.Vectors[0]))
		}
	}
	for i, t := range e.Texts {
		if t == "" {
			return fmt.Errorf("texts[%d] is empty", i)
		}
	}
	key := EmbedFixtureKey(e.Texts)
	if err := f.claim(key, file); err != nil {
		return err
	}
	f.embeddings[key] = e
	return nil
}

func (f *Fixtures) claim(key, file string) error {
	if where, ok := f.files[key]; ok {
		if where == "" {
			return fmt.Errorf("this request is already recorded (%s)", key)
		}
		return fmt.Errorf("this request is already recorded in %s (%s)", where, key)
	}
	f.files[key] = file
	return nil
}

// FixtureKey is what a completion is recorded under: the tier and the request,
// canonicalised. The token budget is part of the key, and a schema is
// canonicalised first, so the same shape written differently keys the same.
func FixtureKey(tier Tier, req Request) string {
	type canonical struct {
		Tier      Tier      `json:"tier"`
		System    string    `json:"system"`
		Messages  []Message `json:"messages"`
		MaxTokens int       `json:"max_tokens"`
		Schema    *Schema   `json:"schema,omitempty"`
	}
	c := canonical{Tier: tier, System: req.System, Messages: req.Messages, MaxTokens: req.MaxTokens}
	if req.Schema != nil {
		schema := *req.Schema
		schema.Definition = canonicalJSON(schema.Definition)
		c.Schema = &schema
	}
	return hashJSON(c)
}

// EmbedFixtureKey is what an embedding call is recorded under: the texts, in
// order, because that is the whole of the request.
func EmbedFixtureKey(texts []string) string {
	return hashJSON(struct {
		Tier  Tier     `json:"tier"`
		Texts []string `json:"texts"`
	}{Tier: TierEmbed, Texts: texts})
}

func hashJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "sha256:unencodable"
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NewFake builds a registry that answers from fx. Every tier the configuration
// names is served, whatever provider it names. A request nothing was recorded
// for is [ErrNoFixture], naming the key to record.
func NewFake(cfg Config, fx *Fixtures, opts ...Option) (Registry, error) {
	if fx == nil {
		fx = NewFixtures()
	}
	var providers []Provider
	for _, tier := range cfg.Configured() {
		name := cfg.Tiers[tier].Provider
		if slices.ContainsFunc(providers, func(p Provider) bool { return p.Name() == name }) {
			continue
		}
		providers = append(providers, &fakeProvider{name: name, fixtures: fx})
	}
	return NewRegistry(cfg, providers, opts...)
}

type fakeProvider struct {
	name     string
	fixtures *Fixtures
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Capabilities() Capabilities {
	return Capabilities{Complete: true, Embed: true, SystemPrompt: true, StructuredOutput: true}
}

func (p *fakeProvider) Completer(b Build) (Completer, error) {
	return &fakeCompleter{tier: b.Tier, fixtures: p.fixtures}, nil
}

func (p *fakeProvider) Embedder(b Build) (Embedder, error) {
	return &fakeEmbedder{dimensions: b.Config.Dimensions, fixtures: p.fixtures}, nil
}

type fakeCompleter struct {
	tier     Tier
	fixtures *Fixtures
}

func (c *fakeCompleter) Complete(_ context.Context, req Request) (Response, error) {
	key := FixtureKey(c.tier, req)
	fixture, ok := c.fixtures.completions[key]
	if !ok {
		schema := "no schema"
		if req.Schema != nil {
			schema = req.Schema.Name
		}
		return Response{}, fmt.Errorf("%w: %s tier, %d messages, %d tokens, %s: record %s and check it in",
			ErrNoFixture, c.tier, len(req.Messages), req.MaxTokens, schema, key)
	}
	return fixture.Response, nil
}

type fakeEmbedder struct {
	dimensions int
	fixtures   *Fixtures
}

func (e *fakeEmbedder) Dimensions() int { return e.dimensions }

func (e *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vectors, _, err := e.EmbedTokens(ctx, texts)
	return vectors, err
}

func (e *fakeEmbedder) EmbedTokens(_ context.Context, texts []string) ([][]float32, Tokens, error) {
	key := EmbedFixtureKey(texts)
	fixture, ok := e.fixtures.embeddings[key]
	if !ok {
		return nil, Tokens{}, fmt.Errorf("%w: %s tier, %d texts: record %s and check it in", ErrNoFixture, TierEmbed, len(texts), key)
	}
	out := make([][]float32, len(fixture.Vectors))
	for i, v := range fixture.Vectors {
		out[i] = slices.Clone(v)
	}
	return out, fixture.Tokens, nil
}
