// Package anthropic is the Anthropic adapter: the Messages API behind
// internal/llm's [llm.Provider], and the only place in Hearsay that knows what
// an Anthropic request looks like (ADR-0005).
//
// It is deliberately thin. Retries, backoff, rate-limit waits, timeouts and
// token accounting belong to the registry that wraps it, so what is here is one
// HTTP call, the translation in each direction, and enough of the provider's
// error vocabulary to say whether trying again could work.
//
// Anthropic has no embedding model, which is why the embed tier points at
// another provider from the first day (ADR-0005). That is declared as a
// capability rather than discovered on the first call.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
)

const (
	// DefaultBaseURL is the API. A tier's `base_url` replaces it, which is how
	// an operator puts a gateway in front of the provider and how a test points
	// one at a server replaying recorded responses.
	DefaultBaseURL = "https://api.anthropic.com"
	// APIKeyEnv is where the credential comes from unless a tier names another
	// variable. Credentials are never in the configuration repository
	// (ADR-0005).
	APIKeyEnv = "ANTHROPIC_API_KEY"
	// apiVersion is the version header every request carries. It is pinned:
	// the shape this adapter reads is the shape this version returns.
	apiVersion = "2023-06-01"
	// maxResponseBytes bounds what one answer may be, so that a provider or a
	// gateway that streams something enormous cannot exhaust a worker.
	maxResponseBytes = 32 << 20
)

// Provider is the adapter. It holds nothing: what a call needs comes from the
// [llm.Build] the registry constructs a client with.
type Provider struct{}

// New is the provider to hand to [llm.NewRegistry].
func New() *Provider { return &Provider{} }

// Name implements [llm.Provider].
func (p *Provider) Name() string { return llm.ProviderAnthropic }

// Capabilities implements [llm.Provider]. Structured output is a tool call with
// the caller's schema as its input schema, which the model is required to use;
// there is no embedding model, so this provider cannot back the embed tier and
// the registry refuses to build one on it.
func (p *Provider) Capabilities() llm.Capabilities {
	return llm.Capabilities{Complete: true, Embed: false, SystemPrompt: true, StructuredOutput: true}
}

// Completer implements [llm.Provider].
func (p *Provider) Completer(b llm.Build) (llm.Completer, error) {
	key, err := b.APIKey(APIKeyEnv)
	if err != nil {
		return nil, err
	}
	base := b.Config.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return &client{
		tier:        b.Tier,
		model:       b.Config.Model,
		temperature: b.Config.Temperature,
		endpoint:    strings.TrimSuffix(base, "/") + "/v1/messages",
		key:         key,
		http:        b.HTTP(),
	}, nil
}

// Embedder implements [llm.Provider]. The registry does not call it — it reads
// [Provider.Capabilities] first — but a caller building an adapter by hand gets
// the same answer rather than a nil client.
func (p *Provider) Embedder(llm.Build) (llm.Embedder, error) {
	return nil, fmt.Errorf("%s has no embedding model: point the embed tier at a provider that has one", llm.ProviderAnthropic)
}

// client is one tier's Anthropic client.
type client struct {
	tier        llm.Tier
	model       string
	temperature *float64
	endpoint    string
	key         string
	http        *http.Client
}

// Complete implements [llm.Completer]: one Messages call, one answer.
func (c *client) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	body, err := json.Marshal(c.request(req))
	if err != nil {
		return llm.Response{}, c.err(0, "", fmt.Sprintf("encoding the request: %v", err), false, 0, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, c.err(0, "", fmt.Sprintf("building the request: %v", err), false, 0, err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "application/json")
	httpReq.Header.Set("anthropic-version", apiVersion)
	httpReq.Header.Set("x-api-key", c.key)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A request that never got an answer may get one next time, and a
		// context that is done stops the retry loop before it tries.
		return llm.Response{}, c.err(0, "", "the request did not complete", true, 0, err)
	}
	defer resp.Body.Close()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return llm.Response{}, c.err(resp.StatusCode, "", "reading the answer", true, retryAfter(resp.Header), err)
	}
	if resp.StatusCode != http.StatusOK {
		return llm.Response{}, c.apiError(resp, answer)
	}
	return c.response(req, answer)
}

// messagesRequest is the Messages API's request. Every field of it comes from
// the [llm.Request] or the tier's configuration: nothing about this shape is
// visible to a caller, which is the point of the abstraction.
type messagesRequest struct {
	Model       string      `json:"model"`
	MaxTokens   int         `json:"max_tokens"`
	System      string      `json:"system,omitempty"`
	Messages    []message   `json:"messages"`
	Temperature *float64    `json:"temperature,omitempty"`
	Tools       []tool      `json:"tools,omitempty"`
	ToolChoice  *toolChoice `json:"tool_choice,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type toolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// request translates a call.
//
// Structured output is a tool the model is required to call, whose input schema
// is the caller's schema: the answer then arrives already parsed by the
// provider, and the abstraction validates it against the same schema before the
// caller sees it. Parallel tool use is disabled so that there is exactly one
// answer to read.
func (c *client) request(req llm.Request) messagesRequest {
	out := messagesRequest{
		Model:       c.model,
		MaxTokens:   req.MaxTokens,
		System:      req.System,
		Temperature: c.temperature,
		Messages:    make([]message, len(req.Messages)),
	}
	for i, m := range req.Messages {
		out.Messages[i] = message{Role: string(m.Role), Content: m.Text}
	}
	if req.Schema != nil {
		out.Tools = []tool{{
			Name:        req.Schema.Name,
			Description: req.Schema.Description,
			InputSchema: req.Schema.Definition,
		}}
		out.ToolChoice = &toolChoice{Type: "tool", Name: req.Schema.Name, DisableParallelToolUse: true}
	}
	return out
}

// messagesResponse is what comes back.
type messagesResponse struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    []contentBlock `json:"content"`
	Usage      usage          `json:"usage"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// response translates an answer back into terms no provider owns.
func (c *client) response(req llm.Request, body []byte) (llm.Response, error) {
	var decoded messagesResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return llm.Response{}, c.err(http.StatusOK, "", "the answer is not the JSON this API returns", false, 0, err)
	}
	out := llm.Response{
		Model:      decoded.Model,
		StopReason: stopReason(decoded.StopReason),
		Tokens:     llm.Tokens{Input: decoded.Usage.InputTokens, Output: decoded.Usage.OutputTokens},
	}
	if out.Model == "" {
		out.Model = c.model
	}

	if req.Schema == nil {
		var text strings.Builder
		for _, block := range decoded.Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		out.Text = text.String()
		if out.Text == "" && out.StopReason != llm.StopMaxTokens && out.StopReason != llm.StopRefusal {
			return llm.Response{}, c.err(http.StatusOK, "", "the answer has no text in it", false, 0, nil)
		}
		return out, nil
	}

	i := slices.IndexFunc(decoded.Content, func(b contentBlock) bool {
		return b.Type == "tool_use" && b.Name == req.Schema.Name
	})
	if i < 0 {
		if out.StopReason == llm.StopMaxTokens || out.StopReason == llm.StopRefusal {
			// The registry turns a truncated structured answer into
			// llm.ErrTruncated, and a refusal is the model's answer rather
			// than a failure of this adapter's.
			return out, nil
		}
		return llm.Response{}, c.err(http.StatusOK, "", fmt.Sprintf("the answer did not use the %s tool it was told to use", req.Schema.Name), false, 0, nil)
	}
	out.JSON = decoded.Content[i].Input
	return out, nil
}

// stopReason translates the provider's vocabulary. A reason this build does not
// know is [llm.StopOther] rather than an error: a provider adding one must not
// stop an answer being returned.
func stopReason(s string) llm.StopReason {
	switch s {
	case "end_turn", "tool_use", "stop_sequence":
		return llm.StopEnd
	case "max_tokens":
		return llm.StopMaxTokens
	case "refusal":
		return llm.StopRefusal
	default:
		return llm.StopOther
	}
}

// errorBody is the shape of a refused request.
type errorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// apiError turns a refusal into an [*llm.Error], which is what the registry's
// retry loop reads. What is retryable is the provider's own list: too many
// requests, an overloaded server, and the 5xx family. Everything else — a bad
// request, a credential that is not accepted, a model that does not exist — is
// a call that will fail the same way however often it is made.
func (c *client) apiError(resp *http.Response, body []byte) error {
	var decoded errorBody
	_ = json.Unmarshal(body, &decoded)
	kind, msg := decoded.Error.Type, decoded.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 512 {
			msg = msg[:512] + "…"
		}
	}
	// 5xx covers the overloaded_error the API answers with under load, which
	// is 529.
	retryable := resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusRequestTimeout ||
		resp.StatusCode >= 500 && resp.StatusCode <= 599
	return c.err(resp.StatusCode, kind, msg, retryable, retryAfter(resp.Header), nil)
}

// retryAfter reads the header a rate-limited provider asks to be left alone
// with. Anthropic sends whole seconds; anything else is ignored, and the
// registry falls back to its own backoff.
func retryAfter(h http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(h.Get("retry-after")))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// err builds this provider's error, with what the registry needs to decide
// whether to try again. It carries no request content: an error string reaches
// a log line, and a prompt in a log line is L1 text in a log line (ADR-0008).
func (c *client) err(status int, kind, msg string, retryable bool, after time.Duration, wrapped error) error {
	return &llm.Error{
		Provider:   llm.ProviderAnthropic,
		Model:      c.model,
		Tier:       c.tier,
		Status:     status,
		Kind:       kind,
		Msg:        msg,
		Retryable:  retryable,
		RetryAfter: after,
		Err:        wrapped,
	}
}

// The adapter satisfies the interfaces the registry builds from.
var (
	_ llm.Provider  = (*Provider)(nil)
	_ llm.Completer = (*client)(nil)
)
