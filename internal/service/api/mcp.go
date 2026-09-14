package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/kpenfound/hearsay/internal/version"
)

// MCPProtocolVersion is the MCP revision this server speaks when a client asks
// for one it does not know. It is the revision whose Streamable HTTP transport
// lets a server answer a POST with a single JSON response.
const MCPProtocolVersion = "2025-06-18"

// mcpVersions are the revisions whose tool calls look like this one's.
var mcpVersions = []string{"2025-03-26", MCPProtocolVersion}

// The JSON-RPC error codes this server answers with.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ToolContent is one piece of a tools/call result.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult is a tools/call result. Its one text content is the bytes the same
// call serves over HTTP, whether it succeeded or not.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError"`
}

// MCP is the Model Context Protocol surface: JSON-RPC over the Streamable HTTP
// transport, answering every POST with one JSON response and offering no
// server-initiated stream, which the transport allows. It speaks what a client
// needs to call tools — initialize, ping, tools/list and tools/call — and every
// tool is a call of [Calls], so there is nothing a tool does that the HTTP
// surface does differently.
//
// It is written against the protocol rather than a library: the protocol is
// four methods here, and a dependency for four methods is not one that carries
// its weight (CONTRIBUTING.md).
func MCP(calls *Calls) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, fail(http.StatusMethodNotAllowed, "this server offers no event stream: send JSON-RPC as a POST"))
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequest))
		if err != nil {
			writeError(w, fail(http.StatusRequestEntityTooLarge, "the request body is over %d bytes", MaxRequest))
			return
		}
		var req rpcRequest
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{rpcInvalidRequest, "batches are not supported"}})
			return
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{rpcParseError, "the body is not a JSON-RPC message"}})
			return
		}
		if len(req.ID) == 0 {
			// A notification or a response: nothing to answer (initialized,
			// cancelled).
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp := rpcResponse{ID: req.ID}
		switch {
		case req.JSONRPC != "2.0":
			resp.Error = &rpcError{rpcInvalidRequest, `jsonrpc must be "2.0"`}
		case req.Method == "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &params)
			negotiated := MCPProtocolVersion
			if slices.Contains(mcpVersions, params.ProtocolVersion) {
				negotiated = params.ProtocolVersion
			}
			resp.Result = map[string]any{
				"protocolVersion": negotiated,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "hearsay", "version": version.Info().Version},
			}
		case req.Method == "ping":
			resp.Result = struct{}{}
		case req.Method == "tools/list":
			resp.Result = map[string]any{"tools": Tools()}
		case req.Method == "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
				resp.Error = &rpcError{rpcInvalidParams, "tools/call takes a name and arguments"}
				break
			}
			body, err := calls.Call(r.Context(), CallerOf(r.Header), params.Name, params.Arguments)
			if err != nil {
				resp.Result = ToolResult{Content: []ToolContent{{Type: "text", Text: string(ErrorBody(asError(err)))}}, IsError: true}
				break
			}
			resp.Result = ToolResult{Content: []ToolContent{{Type: "text", Text: string(body)}}}
		default:
			resp.Error = &rpcError{rpcMethodNotFound, "no such method"}
		}
		writeRPC(w, resp)
	})
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "encoding the response failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
