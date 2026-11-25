package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func toolNames(tools []api.Tool) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Name
	}
	return names
}

func newToolsHandler(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("HEARSAY_TEST_TOOLS_TOKEN", "test-tools-credential")
	calls, err := api.NewCalls(nil, config.Repo{Principals: []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, TokenEnv: "HEARSAY_TEST_TOOLS_TOKEN"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return api.Handler(calls, nil)
}

func TestToolsAreUniquelyNamed(t *testing.T) {
	names := toolNames(api.Tools())
	if len(names) != len(slices.Compact(slices.Sorted(slices.Values(names)))) {
		t.Fatalf("tool names repeat: %v", names)
	}
	for _, want := range []string{"decide", "delete", "coverage", "search", "watch", "traverse"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools = %v, want %s", names, want)
		}
	}
}

func TestEveryCallIsListedOverHTTPAndMCP(t *testing.T) {
	handler := newToolsHandler(t)
	want := toolNames(api.Tools())

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/", nil))
	var overHTTP struct{ Tools []api.Tool }
	if err := json.Unmarshal(rec.Body.Bytes(), &overHTTP); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/ = %d %s, %v", rec.Code, rec.Body.String(), err)
	}
	if got := toolNames(overHTTP.Tools); !slices.Equal(got, want) {
		t.Errorf("the HTTP call list = %v, want %v", got, want)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set(api.PrincipalHeader, "kyle")
	req.Header.Set(api.AuthorizationHeader, "Bearer test-tools-credential")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var overMCP struct {
		Result struct{ Tools []api.Tool }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &overMCP); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("tools/list = %d %s, %v", rec.Code, rec.Body.String(), err)
	}
	if got := toolNames(overMCP.Result.Tools); !slices.Equal(got, want) {
		t.Errorf("the MCP tool list = %v, want %v", got, want)
	}
}
