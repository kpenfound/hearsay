package push

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Bearer returns the token from exactly one Authorization header, with a
// case-insensitive Bearer scheme and no whitespace or comma in the token.
func Bearer(h http.Header) (string, bool) {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	const prefix = "Bearer "
	value := values[0]
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", false
	}
	token := value[len(prefix):]
	if token == "" || strings.ContainsAny(token, " \t\r\n,") {
		return "", false
	}
	return token, true
}

// ErrEmptyToken marks an unset or empty credential.
var ErrEmptyToken = errors.New("empty token")

// DuplicateTokenError names the principals sharing a token, never the token.
type DuplicateTokenError struct{ Other, ID string }

func (e *DuplicateTokenError) Error() string {
	return fmt.Sprintf("principals %q and %q share an API token", e.Other, e.ID)
}

type credential struct {
	id     string
	digest [sha256.Size]byte
}

// Tokens holds only digests. Populate it at startup, before serving requests;
// its zero value is an empty set. Match may then be called concurrently.
type Tokens struct{ credentials []credential }

// Add registers one principal's token, refusing empty and duplicate tokens.
func (t *Tokens) Add(id, token string) error {
	if token == "" {
		return ErrEmptyToken
	}
	digest := sha256.Sum256([]byte(token))
	for _, c := range t.credentials {
		if subtle.ConstantTimeCompare(digest[:], c.digest[:]) == 1 {
			return &DuplicateTokenError{Other: c.id, ID: id}
		}
	}
	t.credentials = append(t.credentials, credential{id: id, digest: digest})
	return nil
}

// AddEnv loads one principal's credential at startup. A nil lookup uses
// os.LookupEnv. An unset or empty variable wraps ErrEmptyToken.
func (t *Tokens) AddEnv(id, env string, lookup func(string) (string, bool)) error {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, ok := lookup(env)
	if !ok || value == "" {
		return fmt.Errorf("%s is not set or is empty: %w", env, ErrEmptyToken)
	}
	return t.Add(id, value)
}

// Len returns how many credentials were registered.
func (t *Tokens) Len() int { return len(t.credentials) }

// Match compares every configured digest in constant time, without an early
// return when one matches. Empty and unknown tokens authenticate nobody.
func (t *Tokens) Match(token string) (string, bool) {
	got := sha256.Sum256([]byte(token))
	found := ""
	matched := false
	for _, c := range t.credentials {
		if subtle.ConstantTimeCompare(got[:], c.digest[:]) == 1 {
			found, matched = c.id, true
		}
	}
	return found, matched
}
