package l1_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/l1"
)

func TestScrub(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		kinds   []string
		unknown string // a value that must not survive, where naming it is clearer than a want
	}{{
		name:  "text with nothing in it is left alone",
		in:    "The engine takes the lock before it writes, which is why #31 was reverted.",
		want:  "The engine takes the lock before it writes, which is why #31 was reverted.",
		kinds: nil,
	}, {
		name:  "an aws access key",
		in:    "set AWS_ACCESS_KEY_ID to AKIAIOSFODNN7EXAMPLE and try again",
		want:  "set AWS_ACCESS_KEY_ID to [redacted:aws-key] and try again",
		kinds: []string{"aws-key"},
	}, {
		name:  "a github token",
		in:    "curl -H 'Authorization: bearer ghp_" + strings.Repeat("a", 36) + "'",
		want:  "curl -H 'Authorization: bearer [redacted:github-token]'",
		kinds: []string{"github-token"},
	}, {
		name:  "a fine-grained github token",
		in:    "github_pat_" + strings.Repeat("B", 40) + " is the one that broke",
		want:  "[redacted:github-token] is the one that broke",
		kinds: []string{"github-token"},
	}, {
		name:  "a slack token",
		in:    "xoxb-not-a-real-token-value rotated",
		want:  "[redacted:slack-token] rotated",
		kinds: []string{"slack-token"},
	}, {
		name:  "a provider api key",
		in:    "export KEY=sk-ant-" + strings.Repeat("x", 32),
		want:  "export KEY=[redacted:api-key]",
		kinds: []string{"api-key"},
	}, {
		name:  "a json web token",
		in:    "cookie: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		want:  "cookie: [redacted:jwt]",
		kinds: []string{"jwt"},
	}, {
		name:  "credentials in a url keep the url",
		in:    "postgres://hearsay:hunter2@db.internal:5432/hearsay is the one",
		want:  "postgres://[redacted:url-credentials]@db.internal:5432/hearsay is the one",
		kinds: []string{"url-credentials"},
	}, {
		name:  "an assignment keeps its key",
		in:    `password = "correct-horse-battery-staple"`,
		want:  "password = [redacted:secret]",
		kinds: []string{"secret"},
	}, {
		name:  "an email address",
		in:    "ask kyle@example.com about it",
		want:  "ask [redacted:email] about it",
		kinds: []string{"email"},
	}, {
		name:  "a private key block, over several lines",
		in:    "here it is:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\nwhatever\n-----END RSA PRIVATE KEY-----\nthanks",
		want:  "here it is:\n[redacted:private-key]\nthanks",
		kinds: []string{"private-key"},
	}, {
		name:  "several shapes at once",
		in:    "mail kyle@example.com the key AKIAIOSFODNN7EXAMPLE",
		want:  "mail [redacted:email] the key [redacted:aws-key]",
		kinds: []string{"aws-key", "email"},
	}, {
		name:  "prose that mentions a token without pasting one",
		in:    "the token is in Vault now, ask before rotating it",
		want:  "the token is in Vault now, ask before rotating it",
		kinds: nil,
	}, {
		name:  "a version number is not a key",
		in:    "bumped to v1.0.0-beta.11 and re-ran the check",
		want:  "bumped to v1.0.0-beta.11 and re-ran the check",
		kinds: nil,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, kinds := l1.Scrub(tt.in)
			if got != tt.want {
				t.Errorf("Scrub() = %q\nwant %q", got, tt.want)
			}
			if !slices.Equal(kinds, tt.kinds) {
				t.Errorf("Scrub() reported %v, want %v", kinds, tt.kinds)
			}
			if tt.unknown != "" && strings.Contains(got, tt.unknown) {
				t.Errorf("Scrub() left %q in the text", tt.unknown)
			}
			// Idempotency is what makes a document rebuilt from L0 compare
			// equal to the row already stored. A pattern that matched its own
			// marker would break it, and a document would be rewritten on
			// every distillation for ever.
			again, kindsAgain := l1.Scrub(got)
			if again != got {
				t.Errorf("Scrub(Scrub(x)) = %q, want %q: the scrub is not idempotent", again, got)
			}
			if len(kindsAgain) != 0 {
				t.Errorf("Scrub(Scrub(x)) reported %v, want nothing: already-scrubbed text has nothing left to take out", kindsAgain)
			}
		})
	}
}

// Every marker the scrub writes has to survive being scrubbed again, which is
// the property the table above checks one case at a time. This checks it for
// the markers together, in the one arrangement a document actually produces:
// several of them in one string.
func TestScrubIsIdempotentOverEveryMarkerAtOnce(t *testing.T) {
	in := strings.Join([]string{
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_" + strings.Repeat("a", 36),
		"xoxb-not-a-real-token-value",
		"sk-" + strings.Repeat("z", 40),
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1",
		"postgres://user:pw123456@host/db",
		`api_key: "abcd1234efgh"`,
		"kyle@example.com",
		"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----",
	}, "\n")

	once, kinds := l1.Scrub(in)
	if len(kinds) < 8 {
		t.Errorf("Scrub() reported %v, want every shape in the input", kinds)
	}
	if twice, _ := l1.Scrub(once); twice != once {
		t.Errorf("Scrub(Scrub(x)) changed the text:\n%q\n%q", once, twice)
	}
	for _, secret := range []string{"AKIAIOSFODNN7EXAMPLE", "hunter2", "pw123456", "abcd1234efgh", "kyle@example.com"} {
		if strings.Contains(once, secret) {
			t.Errorf("Scrub() left %q in the text", secret)
		}
	}
}
