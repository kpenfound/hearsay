package push_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector/push"
)

func TestBearer(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"missing", nil, ""}, {"empty header", []string{""}, ""},
		{"token", []string{"Bearer secret"}, "secret"},
		{"scheme case", []string{"bEaReR secret"}, "secret"},
		{"two headers", []string{"Bearer secret", "Bearer secret"}, ""},
		{"joined headers", []string{"Bearer secret,Bearer other"}, ""},
		{"wrong scheme", []string{"Basic secret"}, ""},
		{"no token", []string{"Bearer "}, ""}, {"short", []string{"Bear"}, ""},
		{"leading space", []string{" Bearer secret"}, ""},
		{"double separator", []string{"Bearer  secret"}, ""},
		{"tab separator", []string{"Bearer\tsecret"}, ""},
		{"space in token", []string{"Bearer a b"}, ""},
		{"tab in token", []string{"Bearer a\tb"}, ""},
		{"CR in token", []string{"Bearer a\rb"}, ""},
		{"LF in token", []string{"Bearer a\nb"}, ""},
		{"trailing space", []string{"Bearer secret "}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, value := range tt.values {
				h.Add("Authorization", value)
			}
			got, ok := push.Bearer(h)
			if got != tt.want || ok != (tt.want != "") {
				t.Fatalf("Bearer() = %q, %v, want %q, %v", got, ok, tt.want, tt.want != "")
			}
		})
	}
}

func TestTokens(t *testing.T) {
	tests := []struct{ name, token, want string }{
		{"first", "one-secret", "one"}, {"last", "two-secret", "two"},
		{"unknown", "other", ""}, {"empty", "", ""}, {"similar", "one-secreu", ""},
	}
	var tokens push.Tokens
	for _, id := range []string{"one", "two"} {
		if err := tokens.Add(id, id+"-secret"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tokens.Match(tt.token)
			if got != tt.want || ok != (tt.want != "") {
				t.Fatalf("Match() = %q, %v, want %q, %v", got, ok, tt.want, tt.want != "")
			}
		})
	}
}

func TestTokenRegistration(t *testing.T) {
	tests := []struct {
		name, value    string
		set, duplicate bool
	}{
		{name: "loaded", value: "secret", set: true},
		{name: "unset"}, {name: "empty", set: true},
		{name: "duplicate", value: "secret", set: true, duplicate: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tokens push.Tokens
			if tt.duplicate {
				if err := tokens.Add("first", "secret"); err != nil {
					t.Fatal(err)
				}
			}
			lookup := func(env string) (string, bool) {
				if env != "POSTER_TOKEN" {
					t.Fatalf("lookup(%q), want POSTER_TOKEN", env)
				}
				return tt.value, tt.set
			}
			err := tokens.AddEnv("poster", "POSTER_TOKEN", lookup)
			switch {
			case tt.duplicate:
				var duplicate *push.DuplicateTokenError
				if !errors.As(err, &duplicate) || duplicate.Other != "first" || duplicate.ID != "poster" {
					t.Fatalf("AddEnv() = %v, want the two duplicate principal ids", err)
				}
			case !tt.set || tt.value == "":
				if !errors.Is(err, push.ErrEmptyToken) {
					t.Fatalf("AddEnv() = %v, want ErrEmptyToken", err)
				}
			case err != nil:
				t.Fatalf("AddEnv() = %v, want nil", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatalf("AddEnv() exposed a token: %v", err)
			}
			wantLen := 0
			if tt.value != "" {
				wantLen = 1
			}
			if tokens.Len() != wantLen {
				t.Fatalf("Len() = %d, want %d", tokens.Len(), wantLen)
			}
		})
	}
}

func TestAnEmptyDirectTokenIsRefused(t *testing.T) {
	var tokens push.Tokens
	if err := tokens.Add("poster", ""); !errors.Is(err, push.ErrEmptyToken) {
		t.Fatalf("Add() = %v, want ErrEmptyToken", err)
	}
	if id, ok := tokens.Match(""); ok {
		t.Fatalf("Match(empty) authenticated %q", id)
	}
}

func TestTokensAreReadFromTheProcessEnvironmentWhenNoLookupIsGiven(t *testing.T) {
	var tokens push.Tokens
	t.Setenv("PUSH_TEST_TOKEN", "from-env")
	if err := tokens.AddEnv("poster", "PUSH_TEST_TOKEN", nil); err != nil {
		t.Fatalf("AddEnv() = %v, want nil", err)
	}
	if id, ok := tokens.Match("from-env"); !ok || id != "poster" {
		t.Fatalf("Match() = %q, %v, want poster, true", id, ok)
	}
	if id, ok := tokens.Match(""); ok {
		t.Fatalf("Match(empty) authenticated %q", id)
	}
}
