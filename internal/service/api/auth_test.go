package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func TestAPIAuthenticatesBeforeCallingOnBothTransports(t *testing.T) {
	t.Setenv("HEARSAY_TEST_HUMAN_AUTH", "test-human-api-credential")
	t.Setenv("HEARSAY_TEST_OTHER_AUTH", "test-other-api-credential")
	t.Setenv("HEARSAY_TEST_AGENT_AUTH", "test-agent-api-credential")
	calls, err := api.NewCalls(nil, config.Repo{Principals: []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, TokenEnv: "HEARSAY_TEST_HUMAN_AUTH"},
		{ID: "sam", Kind: principal.KindHuman, TokenEnv: "HEARSAY_TEST_OTHER_AUTH"},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "HEARSAY_TEST_AGENT_AUTH"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := api.Handler(calls, nil)
	paths := []struct{ path, body string }{
		{"/v1/no_such_call", `{}`},
		{"/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`},
	}
	cases := []struct {
		name   string
		head   http.Header
		status int
	}{
		{"name alone", http.Header{api.PrincipalHeader: {"kyle"}}, http.StatusUnauthorized},
		{"wrong person's token", http.Header{api.PrincipalHeader: {"kyle"}, api.AuthorizationHeader: {"Bearer test-other-api-credential"}}, http.StatusUnauthorized},
		{"human authenticated", http.Header{api.PrincipalHeader: {"kyle"}, api.AuthorizationHeader: {"Bearer test-human-api-credential"}}, http.StatusOK},
		{"agent name alone", http.Header{api.PrincipalHeader: {"kyle"}, api.AuthorizationHeader: {"Bearer test-human-api-credential"}, api.AgentHeader: {"shed"}}, http.StatusUnauthorized},
		{"wrong agent token", http.Header{api.PrincipalHeader: {"kyle"}, api.AuthorizationHeader: {"Bearer test-human-api-credential"}, api.AgentHeader: {"shed"}, api.AgentTokenHeader: {"test-other-api-credential"}}, http.StatusUnauthorized},
		{"agent without human token", http.Header{api.PrincipalHeader: {"kyle"}, api.AgentHeader: {"shed"}, api.AgentTokenHeader: {"test-agent-api-credential"}}, http.StatusUnauthorized},
		{"agent authenticated for human", http.Header{api.PrincipalHeader: {"kyle"}, api.AuthorizationHeader: {"Bearer test-human-api-credential"}, api.AgentHeader: {"shed"}, api.AgentTokenHeader: {"test-agent-api-credential"}}, http.StatusOK},
		{"unused agent token", http.Header{api.PrincipalHeader: {"kyle"}, api.AuthorizationHeader: {"Bearer test-human-api-credential"}, api.AgentTokenHeader: {"test-agent-api-credential"}}, http.StatusUnauthorized},
		{"duplicate human token", http.Header{api.PrincipalHeader: {"kyle"}, api.AuthorizationHeader: {"Bearer test-human-api-credential", "Bearer test-other-api-credential"}}, http.StatusUnauthorized},
	}
	for _, path := range paths {
		for _, tc := range cases {
			t.Run(path.path+"/"+tc.name, func(t *testing.T) {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path.path, strings.NewReader(path.body))
				req.Header = tc.head.Clone()
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				want := tc.status
				if tc.status == http.StatusOK && path.path != "/mcp" {
					want = http.StatusNotFound // passed authentication; the call name is unknown
				}
				if w.Code != want {
					t.Errorf("POST %s = %d %s, want %d", path.path, w.Code, w.Body.String(), want)
				}
			})
		}
	}
}

func TestAPIFailsClosedWithoutDistinctConfiguredSecrets(t *testing.T) {
	const key = "HEARSAY_TEST_AUTH_MISSING"
	t.Setenv(key, "")
	for _, tc := range []struct {
		name       string
		principals []principal.Principal
	}{
		{"no token_env", []principal.Principal{{ID: "kyle", Kind: principal.KindHuman}}},
		{"empty environment variable", []principal.Principal{{ID: "kyle", Kind: principal.KindHuman, TokenEnv: key}}},
		{"shared token", []principal.Principal{{ID: "kyle", Kind: principal.KindHuman, TokenEnv: "HEARSAY_TEST_SHARED_A"}, {ID: "sam", Kind: principal.KindHuman, TokenEnv: "HEARSAY_TEST_SHARED_B"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "shared token" {
				t.Setenv("HEARSAY_TEST_SHARED_A", "shared-test-credential")
				t.Setenv("HEARSAY_TEST_SHARED_B", "shared-test-credential")
			}
			if _, err := api.NewCalls(nil, config.Repo{Principals: tc.principals}, nil); err == nil {
				t.Fatal("NewCalls accepted unusable API authentication")
			}
		})
	}
}
