package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// mcpProtocolVersion is the newest MCP revision this server speaks. A client
// asking for another revision gets this one back and decides (MCP version
// negotiation).
const mcpProtocolVersion = "2025-06-18"

var mcpVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

// mcpTool maps one MCP tool onto a toolbelt operation.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func object(props map[string]any, required ...string) map[string]any {
	props["request_id"] = map[string]any{"type": "string", "description": "Optional idempotency key: retrying with the same ID never repeats the effect."}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	payloadSchema = map[string]any{"type": "object", "description": "A payload: {schema, data}.", "properties": map[string]any{
		"schema": map[string]any{"type": "string"}, "data": map[string]any{"description": "Any JSON value."},
	}}
	keySchema = map[string]any{"type": "object", "properties": map[string]any{
		"source": map[string]any{"type": "string"}, "external_id": map[string]any{"type": "string"},
	}, "required": []string{"source", "external_id"}}
)

// mcpTools are the builtin toolbelt operations plus `call` for operations
// extensions register. Inputs are the toolbelt's own wire inputs, so MCP and
// the `os` CLI present one surface with one authority path.
var mcpTools = []mcpTool{
	{Name: "read_context", Description: "Read this Work's accumulated context.", InputSchema: object(map[string]any{})},
	{Name: "emit", Description: "Emit a declared event for this Work.", InputSchema: object(map[string]any{
		"type":            map[string]any{"type": "string"},
		"delivery_id":     map[string]any{"type": "string", "description": "Your stable identity for this event; emitting it again is deduplicated."},
		"data":            payloadSchema,
		"correlation_key": keySchema,
		"occurred_at":     map[string]any{"type": "string", "description": "RFC 3339."},
	}, "type", "delivery_id")},
	{Name: "create_work", Description: "Create follow-up child Work in this scope.", InputSchema: object(map[string]any{
		"kind":             map[string]any{"type": "string"},
		"id":               map[string]any{"type": "string"},
		"correlation_keys": map[string]any{"type": "array", "items": keySchema},
		"context":          payloadSchema,
	}, "kind")},
	{Name: "wait", Description: "Suspend this Work until an event matching the types and/or correlation keys arrives, or the timeout passes. Finish your turn after calling it.", InputSchema: object(map[string]any{
		"match": map[string]any{"type": "object", "properties": map[string]any{
			"types": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"keys":  map[string]any{"type": "array", "items": keySchema},
		}},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1},
		"after_sequence":  map[string]any{"type": "string"},
		"description":     map[string]any{"type": "string", "description": "What you are waiting for, in a few words (\"a reply in the thread\"). Shown to operators; its first line is kept, cut to 200 bytes."},
	}, "match")},
	{Name: "declare_outcome", Description: "Declare how this attempt ends (completed, failed or handoff); it applies when your turn finishes.", InputSchema: object(map[string]any{
		"kind":    map[string]any{"type": "string", "enum": []string{"completed", "failed", "handoff"}},
		"summary": map[string]any{"type": "string"},
		"data":    payloadSchema,
	}, "kind")},
	{Name: "call", Description: "Call an operation an extension registered on the toolbelt (for example github.issue.comment). A file_path in the input is read and sent as content_base64; a downloaded file is written to the input's save_to.", InputSchema: object(map[string]any{
		"operation": map[string]any{"type": "string"},
		"input":     map[string]any{"type": "object", "description": "The operation's input object, for example {\"body\": \"…\"}."},
	}, "operation")},
}

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

// mcp serves the toolbelt as an MCP server over stdio (newline-delimited
// JSON-RPC 2.0) until stdin closes. The credential is re-read for every call,
// so an adopted process keeps working after its token file is replaced.
func (t *toolbelt) mcp(args []string) int {
	if len(args) != 0 {
		return t.usage("mcp takes no arguments")
	}
	in := bufio.NewScanner(t.stdin)
	in.Buffer(make([]byte, 64*1024), 8<<20)
	enc := json.NewEncoder(t.out)
	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue // notifications (initialized, cancelled) need no answer
		}
		result, rerr := t.mcpHandle(req)
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
		if rerr == nil && result == nil {
			resp.Result = map[string]any{}
		}
		if err := enc.Encode(resp); err != nil {
			return 1
		}
	}
	if err := in.Err(); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(t.errOut, "os: mcp input unreadable")
		return 1
	}
	return 0
}

func (t *toolbelt) mcpHandle(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mcpProtocolVersion
		if mcpVersions[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "open-conduit", "version": "1"},
			"instructions":    "The Open Conduit toolbelt for this execution: read this Work's context, emit events, create child Work, wait for events and declare the outcome.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		return t.mcpCall(p.Name, p.Arguments), nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found"}
}

// mcpCall performs one tool call. Toolbelt refusals are tool results with
// isError (safe error code only), not protocol errors, so the agent sees them.
func (t *toolbelt) mcpCall(name string, arguments json.RawMessage) map[string]any {
	fail := func(msg string) map[string]any {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}
	}
	known := false
	for _, tool := range mcpTools {
		known = known || tool.Name == name
	}
	if !known {
		return fail("unknown tool")
	}
	var args map[string]json.RawMessage
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return fail("arguments must be an object")
		}
	}
	call := client.ToolCall{Operation: name}
	if raw, ok := args["request_id"]; ok {
		if json.Unmarshal(raw, &call.RequestID) != nil {
			return fail("request_id must be a string")
		}
		delete(args, "request_id")
	}
	switch name {
	case "call":
		if json.Unmarshal(args["operation"], &call.Operation) != nil || call.Operation == "" {
			return fail("call requires an operation")
		}
		if input, ok := args["input"]; ok && string(input) != "null" {
			wrapped, err := callInput(input)
			if err != nil {
				return fail(err.Error())
			}
			call.Input = wrapped
		}
	case "read_context":
	default:
		if len(args) > 0 {
			b, err := json.Marshal(args)
			if err != nil {
				return fail("invalid arguments")
			}
			call.Input = b
		}
	}
	c, err := t.client()
	if err != nil {
		return fail(err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := c.ToolbeltCall(ctx, call)
	if err != nil {
		var api *client.Error
		if errors.As(err, &api) {
			return fail(api.Error())
		}
		return fail("management endpoint unreachable")
	}
	var shown any
	switch name {
	case "read_context":
		shown = result.Context
		if shown == (*client.Payload)(nil) {
			shown = client.Payload{}
		}
	case "emit":
		shown = result.Event
	case "create_work":
		shown = result.Work
	case "wait":
		shown = result.Wait
	case "declare_outcome":
		shown = map[string]string{"declared": "ok"}
	default:
		shown = result.Output
		if shown == (*client.Payload)(nil) {
			shown = client.Payload{}
		} else if shown, err = fileOutput(result.Output); err != nil {
			return fail(err.Error())
		}
	}
	text, _ := json.MarshalIndent(shown, "", "  ")
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "isError": false}
}

// callInput is a `call` tool input as the toolbelt wire expects it: the
// operation's input object becomes the payload's data (an explicit
// {schema, data} payload passes through), with file_path filled in.
func callInput(input json.RawMessage) (json.RawMessage, error) {
	var probe map[string]json.RawMessage
	if json.Unmarshal(input, &probe) == nil {
		_, hasData := probe["data"]
		_, hasSchema := probe["schema"]
		if hasData && len(probe) <= 2 && (len(probe) == 1 || hasSchema) {
			data, err := fileInput(probe["data"])
			if err != nil {
				return nil, err
			}
			probe["data"] = data
			return json.Marshal(probe)
		}
	}
	data, err := fileInput(input)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"data": data})
}
