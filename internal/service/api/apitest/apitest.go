// Package apitest holds the test support for speaking MCP to the API.
package apitest

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/hearsay/internal/service/api"
)

// ToolCall is the JSON-RPC tools/call request for one call with its arguments.
func ToolCall(t testing.TB, call string, args any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": call, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// ToolText is the one text content of a tools/call answer, which is the bytes
// the same call serves over HTTP, and whether the call failed. ok is false when
// the answer is not a result holding exactly one content.
func ToolText(body []byte) (text string, isError, ok bool) {
	var rpc struct {
		Result api.ToolResult `json:"result"`
	}
	if json.Unmarshal(body, &rpc) != nil || len(rpc.Result.Content) != 1 {
		return "", false, false
	}
	return rpc.Result.Content[0].Text, rpc.Result.IsError, true
}
