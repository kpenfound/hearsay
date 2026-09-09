package llm

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrTierNotConfigured is a tier no configuration names. The embed tier is
	// the one this happens to in practice: there is no shipped embedding
	// provider yet, so a configuration that does not name one has no embedder.
	ErrTierNotConfigured = errors.New("model tier is not configured")
	// ErrTruncated is an answer cut off at the token budget. It is returned
	// rather than a parse failure when a schema was asked for, because
	// truncated JSON is a budget problem and reads like a broken model.
	ErrTruncated = errors.New("the answer was cut off at the token budget")
	// ErrRefused is a model declining to answer. It is the model's answer
	// rather than a failure of the call, and it is an error because it is
	// never the answer the caller asked for.
	ErrRefused = errors.New("the model declined to answer")
	// ErrDimensions is an embed tier whose vectors are not the width the
	// column holds. See [CheckDimensions].
	ErrDimensions = errors.New("embedding width does not match the column")
	// ErrNoFixture is the fake being asked something nothing was recorded for.
	// It is the error a test that would otherwise call a provider gets.
	ErrNoFixture = errors.New("no recorded fixture for this request")
)

// Error is a call that a provider refused or could not answer, in terms the
// abstraction can act on: whether trying again could work, and how long to wait
// before it does. Adapters return it; the retry loop reads it with [errors.As];
// callers mostly just print it.
//
// It carries no request content — a message that quoted the prompt would put
// L1 text in a log line, which is access control rather than style (ADR-0008).
type Error struct {
	// Provider and Model are what was called, and Tier is what for.
	Provider string
	Model    string
	Tier     Tier
	// Status is the HTTP status where there was one, and 0 otherwise.
	Status int
	// Kind is the provider's own name for the failure, kept as text because it
	// is a provider's vocabulary and this package does not model it.
	Kind string
	// Msg is what the provider said, or what went wrong locally.
	Msg string
	// Retryable is whether the same call could succeed later.
	Retryable bool
	// RetryAfter is how long the provider asked to be left alone, zero if it
	// did not say.
	RetryAfter time.Duration
	// Err is the underlying failure, where one was wrapped.
	Err error
}

// Error renders what failed, not that it failed.
func (e *Error) Error() string {
	parts := []string{fmt.Sprintf("%s tier: %s", e.Tier, e.Provider)}
	if e.Model != "" {
		parts[0] += " " + e.Model
	}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.Kind != "" {
		parts = append(parts, e.Kind)
	}
	msg := e.Msg
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg != "" {
		parts = append(parts, msg)
	}
	return strings.Join(parts, ": ")
}

// Unwrap exposes the underlying failure, so a caller can still find a
// [context.DeadlineExceeded] under a call that timed out.
func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether trying err again could work. Anything that is not
// an [*Error] is not retried: an error this package does not recognise is one
// it cannot say that about.
func Retryable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Retryable
}
