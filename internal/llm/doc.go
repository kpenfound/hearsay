// Package llm is the model provider abstraction, with three named tiers:
// distill, assert and embed (ADR-0005).
//
// Callers ask for a tier and get a client. Which provider and which model back
// a tier is configuration, not code, and no provider shape — no Anthropic tool
// block, no OpenAI response format — may leak out of this package. Structured
// output is part of the abstraction: a caller supplies a JSON schema and gets
// JSON that validates against it.
//
// See docs/adr/0005-llm-provider-abstraction-with-three-model-tiers.md.
package llm
