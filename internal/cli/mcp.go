package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
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
	{Name: "create_work", Description: "Hand a task to another agent as child Work in this scope, of a kind your grant lists. When it ends you receive a core.work.child.ended event with its result; wait for it.", InputSchema: object(map[string]any{
		"kind":             map[string]any{"type": "string", "description": "A kind of child Work your toolbelt guidance lists; the host refuses any other."},
		"id":               map[string]any{"type": "string"},
		"correlation_keys": map[string]any{"type": "array", "items": keySchema},
		"context":          map[string]any{"type": "object", "description": "The task, as a payload: {schema, data}, for example {\"schema\": \"<kind>.v1\", \"data\": {\"task\": \"…\"}}.", "properties": payloadSchema["properties"]},
	}, "kind")},
	{Name: "cancel_work", Description: "Cancel child Work you asked for with create_work that hasn't ended yet, when you no longer need its answer. You still receive its core.work.child.ended event, in state cancelled.", InputSchema: object(map[string]any{
		"work_id": map[string]any{"type": "string", "description": "The child Work's ID, as create_work returned it."},
		"reason":  map[string]any{"type": "string", "description": "Why you no longer need it, in a few words; recorded on the child's history."},
	}, "work_id")},
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
			"instructions":    t.mcpInstructions(),
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

const mcpBaseInstructions = "The Open Conduit toolbelt for this execution: read this Work's context, emit events, create child Work, wait for events and declare the outcome."

// mcpInstructions are the server's instructions: the base sentence, the
// operations this execution may call through `call` with each one's
// summary and input fields, then the guidance of each, which the extension
// owning the operation supplies. Without them (an older daemon, an
// unreachable endpoint) the base sentence still serves.
func (t *toolbelt) mcpInstructions() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var guide client.ToolbeltGuide
	err := t.withRecovery(ctx, func(c *client.Client) (e error) {
		guide, e = c.ToolbeltGuide(ctx)
		return e
	})
	if err != nil {
		return mcpBaseInstructions
	}
	parts := []string{mcpBaseInstructions}
	if ops := mcpOperations(guide.Operations); ops != "" {
		parts = append(parts, ops)
	}
	parts = append(parts, guide.Guidance...)
	return strings.Join(parts, "\n\n")
}

// mcpOperations lists the granted operations for the instructions, one line
// each: its name, summary and input fields, an optional one marked with ?
// (`body`, `number`?). The reference page has each field's meaning.
func mcpOperations(ops []client.OperationDoc) string {
	if len(ops) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Operations\n\nCall these with the `call` tool, giving the `operation` and its `input` object:\n")
	for _, op := range ops {
		fmt.Fprintf(&b, "\n- `%s`", op.Name)
		if op.Summary != "" {
			b.WriteString(": " + op.Summary)
		}
		if fields := inputFields(op.Input); fields != "" {
			b.WriteString(" Input: " + fields + ".")
		}
	}
	return b.String()
}

// inputFields are a JSON Schema object's properties in order, required ones
// first, each optional one marked with ?; "" for none or a schema that
// isn't an object.
func inputFields(schema json.RawMessage) string {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil || len(s.Properties) == 0 {
		return ""
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if required[names[i]] != required[names[j]] {
			return required[names[i]]
		}
		return names[i] < names[j]
	})
	for i, name := range names {
		names[i] = "`" + name + "`"
		if !required[name] {
			names[i] += "?"
		}
	}
	return strings.Join(names, ", ")
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
	if name == "call" {
		if op, builtin := builtinCall(args); builtin {
			// A builtin reached through call does exactly what its own tool
			// does: the same input on the wire and the same part of the result
			// shown (read_context's context, emit's event), never call's empty
			// output.
			rerouted, err := builtinArguments(args)
			if err != nil {
				return fail(err.Error())
			}
			return t.mcpCall(op, rerouted)
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
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	result, err := t.toolbeltCall(ctx, call)
	if err != nil {
		return fail(callError(err))
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
	case "create_work", "cancel_work":
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

// builtinCall reports the operation of a `call` tool call and whether it
// names one of the builtin tools (read_context, emit, wait, …) rather than an
// operation an extension registered.
func builtinCall(args map[string]json.RawMessage) (string, bool) {
	var op string
	if json.Unmarshal(args["operation"], &op) != nil || op == "call" {
		return op, false
	}
	for _, tool := range mcpTools {
		if tool.Name == op {
			return op, true
		}
	}
	return op, false
}

// builtinArguments are a `call` tool call's arguments as the builtin tool it
// names takes them: the input object's fields, with the call's request_id.
func builtinArguments(args map[string]json.RawMessage) (json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if input, ok := args["input"]; ok && string(input) != "null" {
		if err := json.Unmarshal(input, &out); err != nil {
			return nil, errors.New("call input must be an object")
		}
		if out == nil {
			out = map[string]json.RawMessage{}
		}
	}
	if raw, ok := args["request_id"]; ok {
		out["request_id"] = raw
	}
	return json.Marshal(out)
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

// BuiltinOperation is one builtin toolbelt operation as the MCP server
// presents it, for the generated operations reference page.
type BuiltinOperation struct {
	Name, Description string
	InputSchema       map[string]any
}

// BuiltinOperations are the core's toolbelt operations (every one but
// `call`, which reaches the operations extensions register).
func BuiltinOperations() []BuiltinOperation {
	var out []BuiltinOperation
	for _, t := range mcpTools {
		if t.Name != "call" {
			out = append(out, BuiltinOperation{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	return out
}
