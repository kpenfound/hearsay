package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Registry resolves a tier to a client, per configuration. Callers see tiers,
// not providers: which provider and which model answers a tier is [Config].
//
// The registry applies retries with backoff, rate-limit handling, timeouts and
// token accounting around the provider's own client.
type Registry interface {
	// Completer returns the client for a completion tier. It is an error to
	// ask for [TierEmbed], which no completer serves, and an error to ask for
	// a tier the configuration does not name.
	Completer(Tier) (Completer, error)
	// Embedder returns the client for [TierEmbed].
	Embedder() (Embedder, error)
	// Usage is what has been spent through this registry since it was built,
	// one row per tier, provider and model.
	Usage() []Usage
	// Close releases what the tiers' providers hold between calls, such as the
	// sandboxes claude-sbx keeps warm, and must be called when the process is
	// done with the registry. A completer called after Close fails.
	Close(ctx context.Context) error
}

// Closer is a provider's client that holds something between calls. The
// registry closes every client that is one in [Registry.Close].
type Closer interface {
	Close(ctx context.Context) error
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
	// match the vector(N) column L1 stores them in ([CheckDimensions]). What
	// comes back from [Embed] is held to it.
	Dimensions() int
}

// UsageEmbedder is an [Embedder] whose provider says what a call cost. The
// registry accounts for embedding tokens through it; an adapter whose provider
// reports nothing implements plain [Embedder], and its calls are counted with no
// tokens against them.
type UsageEmbedder interface {
	Embedder
	EmbedTokens(ctx context.Context, texts []string) ([][]float32, Tokens, error)
}

// Role is who a message is from. The system prompt is carried separately, in
// [Request.System].
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
// has to satisfy. Nothing provider-shaped belongs in it.
type Request struct {
	// System is the system prompt. It may be empty.
	System string `json:"system,omitempty"`
	// Messages is the conversation, oldest first. It starts with a
	// [RoleUser] message and alternates.
	Messages []Message `json:"messages"`
	// MaxTokens is the budget for the answer. Zero takes the tier's configured
	// budget.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Schema, when set, asks for JSON satisfying it rather than text. The
	// answer is validated against the schema before it is returned.
	Schema *Schema `json:"schema,omitempty"`
}

// Response is one answer.
type Response struct {
	// ReportedUsage preserves presence, including a reported zero, per
	// attempt. Nil means the provider reports no presence: positive Tokens
	// counts are used and zero is unknown.
	ReportedUsage *JudgmentUsage `json:"reported_usage,omitempty"`
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
	// answer cut off is [ErrTruncated].
	StopMaxTokens StopReason = "max_tokens"
	// StopRefusal is a model declining to answer.
	StopRefusal StopReason = "refusal"
	// StopOther is a reason this abstraction does not model. It is not an
	// error.
	StopOther StopReason = "other"
)

// Tokens is what one call cost. Embedding calls report input tokens only.
type Tokens struct {
	Input  int `json:"input,omitempty"`
	Output int `json:"output,omitempty"`
}

// Validate reports what is wrong with a request, before a provider is asked.
//
// A conversation starts with the caller and alternates.
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
// written to: the vector(N) on L1, whose N is fixed by a migration. The
// distiller calls this at startup against l1.EmbeddingDimensions and refuses to
// run on a mismatch.
func CheckDimensions(e Embedder, column int) error {
	if got := e.Dimensions(); got != column {
		return fmt.Errorf("%w: the embed tier produces %d values and the column holds %d", ErrDimensions, got, column)
	}
	return nil
}
