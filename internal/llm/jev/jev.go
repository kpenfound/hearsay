// Package jev implements TypeSafe's System One typed API, independently of
// completion and embedding providers. Retries and cooldown belong to llm.Judgments.
package jev

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
)

// DefaultBaseURL is TypeSafe's API, never an automatically selected model.
const DefaultBaseURL = "https://api.typesafe.ai"
const maxResponseBytes = 1 << 20

// Client makes one attempt. Its optional HTTPClient is copied so redirects
// cannot carry credentials elsewhere. Configure the client before sharing it.
type Client struct {
	cfg        llm.JudgmentConfig
	key        string
	HTTPClient *http.Client
}

// New constructs a transport without doing IO or reading the environment.
// Production callers use the lazy llm.NewJudgments factory.
func New(cfg llm.JudgmentConfig, key string) *Client {
	return &Client{cfg: cfg.WithDefaults(), key: key}
}

// Judge sends independent typed questions over one shared state. Errors never
// contain the request, response, URL, API key or provider error body.
func (c *Client) Judge(ctx context.Context, r llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	if c.cfg.Validate() != nil || c.cfg.Model == "" || c.key == "" || r.State == "" || !llm.ValidJudgmentRequest(r) {
		return llm.JudgmentResponse{}, failure("invalid_request", 0, false)
	}
	body, err := requestBody(c.cfg, r)
	if err != nil {
		return llm.JudgmentResponse{}, err
	}
	base := c.cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return llm.JudgmentResponse{}, failure("invalid_request", 0, false)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return llm.JudgmentResponse{}, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Classify before reading: an unreadable rate-limit body must not hide
	// Retry-After, and error bodies are never needed or retained.
	if resp.StatusCode != http.StatusOK {
		switch status := resp.StatusCode; {
		case status == 429 || status == 529:
			e := failure("rate_limited", status, true)
			e.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), time.Now())
			return llm.JudgmentResponse{}, e
		case status == 408 || status >= 500:
			return llm.JudgmentResponse{}, failure("unavailable", status, true)
		case status == 401 || status == 403:
			return llm.JudgmentResponse{}, failure("unauthorized", status, false)
		default:
			return llm.JudgmentResponse{}, failure("rejected", status, false)
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return llm.JudgmentResponse{}, transportError(ctx, err)
	}
	if len(data) > maxResponseBytes {
		return llm.JudgmentResponse{}, llm.ErrJudgmentResponse
	}
	return decode(data, r)
}

func failure(kind string, status int, retry bool) *llm.Error {
	return &llm.Error{Provider: "jev", Kind: kind, Status: status, Retryable: retry, Msg: "judgment request could not be completed"}
}

func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return failure("unavailable", 0, true)
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > uint64(math.MaxInt64/int64(time.Second)) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}
