package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/kpenfound/hearsay/internal/principal"
)

// AuthorizationHeader carries the human's bearer token. An agent call also
// carries the agent's token in AgentTokenHeader.
const (
	AuthorizationHeader = "Authorization"
	AgentTokenHeader    = "Hearsay-Agent-Token"
)

type authenticator struct {
	tokens map[string][sha256.Size]byte
	kinds  map[string]principal.Kind
}

// newAuthenticator resolves secret names once, when the API starts. A missing
// credential is a startup error, never a reason to fall back to trusting a
// caller-supplied name. Teams do not act as callers.
func newAuthenticator(principals []principal.Principal) (*authenticator, error) {
	a := &authenticator{tokens: make(map[string][sha256.Size]byte), kinds: make(map[string]principal.Kind)}
	seen := make(map[[sha256.Size]byte]string)
	for _, p := range principals {
		if p.Kind == principal.KindTeam {
			continue
		}
		if p.TokenEnv == "" {
			return nil, fmt.Errorf("principal %q needs token_env to call the API", p.ID)
		}
		value, ok := os.LookupEnv(p.TokenEnv)
		if !ok || value == "" {
			return nil, fmt.Errorf("principal %q: %s is not set or is empty", p.ID, p.TokenEnv)
		}
		digest := sha256.Sum256([]byte(value))
		if other, exists := seen[digest]; exists {
			return nil, fmt.Errorf("principals %q and %q share an API token", other, p.ID)
		}
		seen[digest] = p.ID
		a.tokens[p.ID] = digest
		a.kinds[p.ID] = p.Kind
	}
	return a, nil
}

// authenticate checks both identities before an HTTP or MCP request can reach
// Calls. The human token grants access as that person; an agent token proves
// which configured agent is acting with that person's delegated credential.
func (a *authenticator) authenticate(h http.Header) (Caller, *Error) {
	if len(h.Values(PrincipalHeader)) != 1 || len(h.Values(AgentHeader)) > 1 {
		return Caller{}, fail(http.StatusUnauthorized, "ambiguous caller headers")
	}
	caller := Caller{Principal: strings.TrimSpace(h.Get(PrincipalHeader)), Agent: strings.TrimSpace(h.Get(AgentHeader))}
	if caller.Principal == "" {
		return Caller{}, fail(http.StatusUnauthorized, "name the person in the %s header", PrincipalHeader)
	}
	if a.kinds[caller.Principal] == "" {
		return Caller{}, fail(http.StatusForbidden, "%q is not a configured principal", caller.Principal)
	}
	if a.kinds[caller.Principal] != principal.KindHuman {
		return Caller{}, fail(http.StatusForbidden, "%q is not a human principal", caller.Principal)
	}
	if !validToken(h.Values(AuthorizationHeader), "Bearer ", a.tokens[caller.Principal]) {
		return Caller{}, fail(http.StatusUnauthorized, "invalid credential for %s", PrincipalHeader)
	}
	if caller.Agent == "" {
		if len(h.Values(AgentTokenHeader)) != 0 {
			return Caller{}, fail(http.StatusUnauthorized, "%s requires %s", AgentTokenHeader, AgentHeader)
		}
		return caller, nil
	}
	if a.kinds[caller.Agent] == "" {
		return Caller{}, fail(http.StatusForbidden, "%q is not a configured principal", caller.Agent)
	}
	if a.kinds[caller.Agent] != principal.KindAgent {
		return Caller{}, fail(http.StatusForbidden, "%q is not an agent principal", caller.Agent)
	}
	if !validToken(h.Values(AgentTokenHeader), "", a.tokens[caller.Agent]) {
		return Caller{}, fail(http.StatusUnauthorized, "invalid credential for %s", AgentHeader)
	}
	return caller, nil
}

func validToken(values []string, prefix string, want [sha256.Size]byte) bool {
	if len(values) != 1 {
		return false
	}
	value := values[0]
	if prefix != "" {
		if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
			return false
		}
		value = value[len(prefix):]
	}
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n,") {
		return false
	}
	got := sha256.Sum256([]byte(value))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
