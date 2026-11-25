//go:build integration

package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/service/api"
)

type client struct {
	server     *httptest.Server
	credential func(id string) string
}

func testCredential(id string) string { return "test-" + id + "-api-credential" }

func reachCredential(id string) string { return "reach-" + id + "-credential" }

type answer struct {
	status  int
	isError bool
	body    string
}

func (c *client) post(t *testing.T, path string, caller api.Caller, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, c.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if caller.Principal != "" {
		req.Header.Set(api.PrincipalHeader, caller.Principal)
		req.Header.Set(api.AuthorizationHeader, "Bearer "+c.credential(caller.Principal))
	}
	if caller.Agent != "" {
		req.Header.Set(api.AgentHeader, caller.Agent)
		req.Header.Set(api.AgentTokenHeader, c.credential(caller.Agent))
	}
	if caller.Session != "" {
		req.Header.Set(api.SessionHeader, caller.Session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, got
}

func (c *client) http(t *testing.T, caller api.Caller, call string, args any) []byte {
	t.Helper()
	status, got := c.post(t, "/v1/"+call, caller, string(mustJSON(t, args)))
	if status != http.StatusOK {
		t.Fatalf("POST /v1/%s = %d %s", call, status, got)
	}
	return got
}

func (c *client) mcp(t *testing.T, caller api.Caller, call string, args any) ([]byte, bool) {
	t.Helper()
	status, got := c.post(t, "/mcp", caller, string(toolCall(t, call, args)))
	if status != http.StatusOK {
		t.Fatalf("POST /mcp = %d %s", status, got)
	}
	var resp struct {
		Result api.ToolResult `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.Unmarshal(got, &resp); err != nil || resp.Error != nil || len(resp.Result.Content) != 1 {
		t.Fatalf("tools/call answered %s", got)
	}
	return []byte(resp.Result.Content[0].Text), resp.Result.IsError
}

func (c *client) both(t *testing.T, caller api.Caller, call string, args any) (overHTTP, overMCP answer) {
	t.Helper()
	status, got := c.post(t, "/v1/"+call, caller, string(mustJSON(t, args)))
	overHTTP = answer{status: status, body: string(got)}
	status, got = c.post(t, "/mcp", caller, string(toolCall(t, call, args)))
	var resp struct {
		Result api.ToolResult `json:"result"`
	}
	if err := json.Unmarshal(got, &resp); err != nil || len(resp.Result.Content) != 1 {
		t.Fatalf("tools/call %s answered %d %s", call, status, got)
	}
	overMCP = answer{status: status, isError: resp.Result.IsError, body: resp.Result.Content[0].Text}
	return overHTTP, overMCP
}

func toolCall(t *testing.T, call string, args any) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": call, "arguments": args},
	})
}

type who struct {
	human, agent string
	humanToken   *string
	agentToken   *string
}

func str(s string) *string { return &s }

func invoke(t *testing.T, base string, credentials map[string]string, mcp bool, c who, name string, args any) (int, []byte, error) {
	t.Helper()
	body := mustJSON(t, args)
	path := "/v1/" + name
	if mcp {
		path = "/mcp"
		body = toolCall(t, name, json.RawMessage(body))
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if c.human != "" {
		req.Header.Set(api.PrincipalHeader, c.human)
		token := credentials[c.human]
		if c.humanToken != nil {
			token = *c.humanToken
		}
		if token != "" {
			req.Header.Set(api.AuthorizationHeader, "Bearer "+token)
		}
	}
	if c.agent != "" {
		req.Header.Set(api.AgentHeader, c.agent)
		token := credentials[c.agent]
		if c.agentToken != nil {
			token = *c.agentToken
		}
		if token != "" {
			req.Header.Set(api.AgentTokenHeader, token)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if !mcp {
		return resp.StatusCode, got, nil
	}
	var rpc struct {
		Result api.ToolResult `json:"result"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(got, &rpc) != nil || len(rpc.Result.Content) != 1 {
		return resp.StatusCode, got, nil
	}
	if rpc.Result.IsError {
		return -1, []byte(rpc.Result.Content[0].Text), nil
	}
	return http.StatusOK, []byte(rpc.Result.Content[0].Text), nil
}
