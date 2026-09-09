package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Registry resolves a tier to a client, per configuration (ADR-0005). Callers
// see tiers, not providers: which provider and which model answers a tier is
// [Config], and swapping one is a configuration edit rather than a code change.
//
// Everything a caller of the returned clients would otherwise have to
// implement — retries with backoff, rate-limit handling, timeouts and token
// accounting — is applied by the registry around the provider's own client, so
// there is exactly one place that knows how a model call behaves.
type Registry interface {
	// Completer returns the client for a completion tier. It is an error to
	// ask for [TierEmbed], which no completer serves, and an error to ask for
	// a tier the configuration does not name.
	Completer(Tier) (Completer, error)
	// Embedder returns the client for [TierEmbed].
	Embedder() (Embedder, error)
	// Usage is what has been spent through this registry since it was built,
	// one row per tier, provider and model. It is the read ADR-0005's "cost is
	// observable per tier by construction" asks for; until the metric provider
	// in internal/telemetry lands (ADR-0008), it is also where those metrics
	// come from.
	Usage() []Usage
}

// Completer turns a prompt into text, or into JSON satisfying a schema.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Embedder turns text into vectors.
type Embedder interface {
	// Embed returns one vector per input, in order, each of [Embedder.Dimensions]
	// values.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimensions is the width of the vectors this tier produces. It has to
	// match the vector(N) column L1 stores them in (ADR-0004), which is what
	// [CheckDimensions] compares it against. What comes back from [Embed] is
	// held to it whether anything checks the column or not.
	Dimensions() int
}

// UsageEmbedder is an [Embedder] whose provider says what a call cost. An
// adapter implements it where it can, and the registry accounts for embedding
// tokens through it; an adapter whose provider reports nothing implements plain
// [Embedder], and its calls are counted with no tokens against them.
//
// It is a second method rather than a change to [Embedder] because the
// interface ADR-0005 fixes is the one callers use, and no caller wants the
// token count: the registry does, on their behalf.
type UsageEmbedder interface {
	Embedder
	EmbedTokens(ctx context.Context, texts []string) ([][]float32, Tokens, error)
}

// Role is who a message is from. There are two: the abstraction carries the
// system prompt separately, in [Request.System], because that is what every
// provider does with it.
type Role string

const (
	// RoleUser is the caller.
	RoleUser Role = "user"
	// RoleAssistant is the model, in a conversation being continued.
	RoleAssistant Role = "assistant"
)

// Message is one turn of a conversation.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text"`
}

// Request is one completion, in terms no provider owns: a system prompt, an
// ordered list of messages, a token budget, and optionally a schema the answer
// has to satisfy. Nothing provider-shaped belongs in it — a caller that needed
// an Anthropic tool block here would be a caller that has to be edited to add a
// second provider, which is the leak ADR-0005 exists to stop.
type Request struct {
	// System is the system prompt. It may be empty; a provider that cannot
	// take one at all is refused when the registry is built, not here.
	System string `json:"system,omitempty"`
	// Messages is the conversation, oldest first. It starts with a
	// [RoleUser] message and alternates: that is the shape every provider
	// accepts, and refusing the others here means one clear error instead of a
	// provider's.
	Messages []Message `json:"messages"`
	// MaxTokens is the budget for the answer. Zero takes the tier's configured
	// budget, which is the usual case; a call site whose prompt needs a bigger
	// one sets it (ADR-0005).
	MaxTokens int `json:"max_tokens,omitempty"`
	// Schema, when set, asks for JSON satisfying it rather than text. How the
	// provider is made to produce it — a tool definition, a response format, a
	// retry on a parse failure — is the adapter's business, and the answer is
	// validated against the schema before it is returned.
	Schema *Schema `json:"schema,omitempty"`
}

// Response is one answer.
type Response struct {
	// Text is the answer, empty on a request that carried a schema.
	Text string `json:"text,omitempty"`
	// JSON is the answer to a request that carried a schema, validated against
	// it. It is nil on a request that did not.
	JSON json.RawMessage `json:"json,omitempty"`
	// StopReason is why the model stopped.
	StopReason StopReason `json:"stop_reason,omitempty"`
	// Model is the model that answered, as the provider reported it. It is not
	// always the model that was asked for: a provider may resolve an alias.
	Model string `json:"model,omitempty"`
	// Tokens is what the call cost.
	Tokens Tokens `json:"tokens,omitzero"`
}

// StopReason is why a model stopped generating, in provider-neutral terms.
type StopReason string

const (
	// StopEnd is a complete answer.
	StopEnd StopReason = "end"
	// StopMaxTokens is an answer cut off at the token budget. A structured
	// answer cut off is truncated JSON, which is why the abstraction turns it
	// into [ErrTruncated] rather than a parse failure.
	StopMaxTokens StopReason = "max_tokens"
	// StopRefusal is a model declining to answer.
	StopRefusal StopReason = "refusal"
	// StopOther is a reason this abstraction does not model. It is not an
	// error: a provider adding a stop reason must not stop the answer being
	// returned.
	StopOther StopReason = "other"
)

// Tokens is what one call cost. Embedding calls report input tokens only.
type Tokens struct {
	Input  int `json:"input,omitempty"`
	Output int `json:"output,omitempty"`
}

// Validate reports what is wrong with a request, before a provider is asked.
//
// The message rules are the common denominator of the providers this
// abstraction targets rather than one provider's: a conversation starts with
// the caller and alternates. Checking them here means a caller sees the same
// error whichever provider a tier points at.
func (r Request) Validate() error {
	if r.MaxTokens < 0 {
		return fmt.Errorf("max tokens is %d: it cannot be negative", r.MaxTokens)
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("a request needs at least one message")
	}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleUser, RoleAssistant:
		default:
			return fmt.Errorf("messages[%d]: no such role %q: want %s or %s", i, m.Role, RoleUser, RoleAssistant)
		}
		if strings.TrimSpace(m.Text) == "" {
			return fmt.Errorf("messages[%d]: the message is empty", i)
		}
		want := RoleUser
		if i%2 == 1 {
			want = RoleAssistant
		}
		if m.Role != want {
			return fmt.Errorf("messages[%d] is from %s: messages start with %s and alternate", i, m.Role, RoleUser)
		}
	}
	if r.Messages[len(r.Messages)-1].Role != RoleUser {
		return fmt.Errorf("the last message is from %s: a request ends with the %s message being answered", RoleAssistant, RoleUser)
	}
	if r.Schema != nil {
		if err := r.Schema.Validate(); err != nil {
			return fmt.Errorf("schema: %w", err)
		}
	}
	return nil
}

// CheckDimensions reports whether an embed tier fits the column its vectors are
// written to: ADR-0004's vector(N) on L1, whose N is fixed by a migration.
// Changing the embed model to one of a different width is that migration plus a
// re-embed of every row, not a configuration edit, which is why ADR-0005 asks
// for the two to be compared before anything is written rather than after.
//
// Nothing calls it yet: the column arrives with search, and the process that
// writes vectors calls this at startup and refuses to run on a mismatch.
func CheckDimensions(e Embedder, column int) error {
	if got := e.Dimensions(); got != column {
		return fmt.Errorf("%w: the embed tier produces %d values and the column holds %d", ErrDimensions, got, column)
	}
	return nil
}
