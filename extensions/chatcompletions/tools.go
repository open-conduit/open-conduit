package chatcompletions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Toolbelt is the toolbelt surface the executor uses (*client.Client over
// the execution's endpoint).
type Toolbelt interface {
	ToolbeltGuide(context.Context) (client.ToolbeltGuide, error)
	ToolbeltCall(context.Context, client.ToolCall) (client.ToolResult, error)
}

// toolset is the attempt's tools: one per granted operation the binding
// offers, by the function name the model calls it with.
type toolset struct {
	tools []Tool
	ops   map[string]string // function name → operation
	// system is the system message: what the toolbelt is, then the
	// guidance of the granted operations.
	system string
}

// systemBase opens every conversation's system message.
const systemBase = "You are working on a task for Open Conduit. Your tools are the operations of its toolbelt for this task: call them to read and change things. End every turn with exactly one call to declare_outcome, or to wait when the task can't go on until something happens."

// fallbackBuiltins are the builtins offered when the daemon names no
// granted operations (an older one): those every agent binding is granted.
var fallbackBuiltins = []string{extension.ReadContext, extension.Wait, extension.DeclareOutcome}

// functionName is the shape every provider accepts for a function name.
var functionName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// buildTools lists the attempt's tools from what the token is granted: the
// builtins (wait and declare_outcome whenever granted, others as the
// binding's operations offer them) and each granted operation the binding
// offers, typed from its input schema when it declares one.
func buildTools(guide client.ToolbeltGuide, c Config) toolset {
	ts := toolset{ops: map[string]string{}}
	granted := guide.Granted
	if granted == nil {
		// An older daemon names no grant: the described operations and
		// the builtins every agent binding has.
		granted = slices.Clone(fallbackBuiltins)
		for _, op := range guide.Operations {
			granted = append(granted, op.Name)
		}
	}
	docs := map[string]client.OperationDoc{}
	for _, op := range guide.Operations {
		docs[op.Name] = op
	}
	add := func(op, description string, params json.RawMessage) {
		name := functionName.ReplaceAllString(strings.ReplaceAll(op, ".", "_"), "_")
		if len(name) > 64 {
			name = name[:64]
		}
		for n := 2; ts.ops[name] != ""; n++ {
			suffix := fmt.Sprintf("_%d", n)
			base := name
			if len(base)+len(suffix) > 64 {
				base = base[:64-len(suffix)]
			}
			name = base + suffix
		}
		ts.ops[name] = op
		ts.tools = append(ts.tools, Tool{Type: "function", Function: FunctionDef{Name: name, Description: description, Parameters: params}})
	}
	for _, op := range builtinOrder {
		if !slices.Contains(granted, op) {
			continue
		}
		if op != extension.Wait && op != extension.DeclareOutcome && !c.offers(op) {
			continue
		}
		b := builtinTools[op]
		add(op, b.description, b.parameters)
	}
	for _, name := range granted {
		if extension.BuiltinOperation(name) || !c.offers(name) {
			continue
		}
		op := docs[name]
		description := op.Summary
		if description == "" {
			description = "The toolbelt operation " + name + "."
		}
		add(name, description, objectSchema(op.Input))
	}
	parts := []string{systemBase}
	parts = append(parts, guide.Guidance...)
	ts.system = strings.Join(parts, "\n\n")
	return ts
}

// objectSchema is an operation's input schema as function parameters,
// which must be an object: an empty one for none.
func objectSchema(raw json.RawMessage) json.RawMessage {
	var probe struct {
		Type json.RawMessage `json:"type"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &probe) != nil || string(probe.Type) != `"object"` {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

type builtinTool struct {
	description string
	parameters  json.RawMessage
}

var builtinOrder = []string{extension.ReadContext, extension.Emit, extension.CreateWork, extension.CancelWork, extension.Wait, extension.DeclareOutcome}

func schema(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

var (
	payloadSchema = map[string]any{"type": "object", "description": "A payload: {schema, data}.", "properties": map[string]any{
		"schema": map[string]any{"type": "string"}, "data": map[string]any{"description": "Any JSON value."},
	}}
	keySchema = map[string]any{"type": "object", "properties": map[string]any{
		"source": map[string]any{"type": "string"}, "external_id": map[string]any{"type": "string"},
	}, "required": []string{"source", "external_id"}}
)

// builtinTools are the builtins as this executor offers them: the wire
// inputs `os mcp` gives Claude Code, with declare_outcome's turn status and
// result as fields of their own.
var builtinTools = map[string]builtinTool{
	extension.ReadContext: {"Read this Work's accumulated context.", schema(map[string]any{"type": "object", "properties": map[string]any{}})},
	extension.Emit: {"Emit a declared event for this Work.", schema(map[string]any{"type": "object", "properties": map[string]any{
		"type":            map[string]any{"type": "string"},
		"delivery_id":     map[string]any{"type": "string", "description": "Your stable identity for this event; emitting it again is deduplicated."},
		"data":            payloadSchema,
		"correlation_key": keySchema,
		"occurred_at":     map[string]any{"type": "string", "description": "RFC 3339."},
	}, "required": []string{"type", "delivery_id"}})},
	extension.CreateWork: {"Hand a task to another agent as child Work in this scope, of a kind your guidance lists. When it ends you receive a core.work.child.ended event with its result; wait for it after this call, never before.", schema(map[string]any{"type": "object", "properties": map[string]any{
		"kind":             map[string]any{"type": "string", "description": "A kind of child Work your guidance lists; the host refuses any other."},
		"id":               map[string]any{"type": "string"},
		"correlation_keys": map[string]any{"type": "array", "items": keySchema},
		"context":          payloadSchema,
	}, "required": []string{"kind"}})},
	extension.CancelWork: {"Cancel child Work you asked for with create_work that hasn't ended yet, when you no longer need its answer.", schema(map[string]any{"type": "object", "properties": map[string]any{
		"work_id": map[string]any{"type": "string", "description": "The child Work's ID, as create_work returned it."},
		"reason":  map[string]any{"type": "string", "description": "Why you no longer need it, in a few words."},
	}, "required": []string{"work_id"}})},
	extension.Wait: {"Park this Work until an event matching the types and/or correlation keys arrives, or the timeout passes. It ends your turn: make every other call first.", schema(map[string]any{"type": "object", "properties": map[string]any{
		"match": map[string]any{"type": "object", "properties": map[string]any{
			"types": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"keys":  map[string]any{"type": "array", "items": keySchema},
		}},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1},
		"description":     map[string]any{"type": "string", "description": "What you are waiting for, in a few words (\"a reply in the thread\"). Shown to operators."},
	}, "required": []string{"match"}})},
	extension.DeclareOutcome: {"End your turn and say how it ended: completed (the task is done), handoff (you've done what you can for now and hand back until something new arrives) or failed. Make every other call first.", schema(map[string]any{"type": "object", "properties": map[string]any{
		"kind":    map[string]any{"type": "string", "enum": []string{"completed", "handoff", "failed"}},
		"summary": map[string]any{"type": "string", "description": "What the turn did, in a sentence or two; it is also your note to yourself for the next turn."},
		"status":  map[string]any{"type": "string", "description": "The status your instructions ask a turn to end in, if they name one (for example review): a lower-case word."},
		"result":  map[string]any{"description": "What you hand back to whoever asked for this Work, when your instructions ask for a result: any JSON value."},
	}, "required": []string{"kind", "summary"}})},
}

// cutOff marks a call its attempt's end cut off, unanswered.
const cutOff = "cut_off"

// maybeRun answers a call an earlier attempt made or was making when it
// ended: the host may have run it, and a new attempt is a new execution,
// whose calls the host doesn't replay, so it isn't made again.
const maybeRun = "unknown: your last attempt ended while this call was in progress, so it may or may not have taken effect. Check (for example by reading what it would have changed) before making it again."

// callOutcome is what a tool call did to the turn: nothing, or ended it
// with a wait or a declared outcome.
type callOutcome struct {
	ended   string // "", extension.Wait or extension.DeclareOutcome
	kind    domain.OutcomeKind
	summary string
	status  string
	result  json.RawMessage
}

// requestID is a tool call's idempotency key: the Work, the turn (which
// never repeats, a fresh conversation included), the call's place in the
// conversation and its ID. The host replays a key only within one
// execution, so it protects a call retried within the attempt; a call an
// ended attempt cut off is never made again (see finishCalls).
func requestID(work domain.ID, turn, index int, id string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s", work, turn, index, id)))
	return "chat-" + hex.EncodeToString(sum[:12])
}

// call performs one tool call through the toolbelt and returns the text the
// model sees and what it did to the turn. A refusal or failure is text the
// model reads, never an attempt failure.
func call(ctx context.Context, tb Toolbelt, ts toolset, tc ToolCall, key string) (string, callOutcome) {
	op, ok := ts.ops[tc.Function.Name]
	if !ok {
		return fmt.Sprintf("error: there is no tool called %q", tc.Function.Name), callOutcome{}
	}
	args := map[string]json.RawMessage{}
	if a := strings.TrimSpace(tc.Function.Arguments); a != "" {
		if err := json.Unmarshal([]byte(a), &args); err != nil {
			return "error: the arguments are not a JSON object (" + truncate(err.Error(), 200) + "); call it again with valid JSON arguments", callOutcome{}
		}
		if args == nil {
			args = map[string]json.RawMessage{}
		}
	}
	var out callOutcome
	request := client.ToolCall{Operation: op, RequestID: key}
	switch op {
	case extension.ReadContext:
	case extension.DeclareOutcome:
		var d struct {
			Kind    string `json:"kind"`
			Summary string `json:"summary"`
			Status  string `json:"status"`
		}
		raw, _ := json.Marshal(args)
		_ = json.Unmarshal(raw, &d)
		if d.Status != "" && !domain.ValidTurnStatus(d.Status) {
			return "error: status must be a lower-case word (letters, digits, - or _)", callOutcome{}
		}
		out = callOutcome{ended: op, kind: domain.OutcomeKind(d.Kind), summary: d.Summary, status: d.Status}
		if r, ok := args["result"]; ok && string(r) != "null" {
			out.result = r
		}
		// The status and result are the executor's to report (core.turn.v1
		// on its result); the host takes the kind and summary.
		wire, _ := json.Marshal(map[string]string{"kind": d.Kind, "summary": d.Summary})
		request.Input = wire
	case extension.Emit, extension.CreateWork, extension.CancelWork, extension.Wait:
		request.Input, _ = json.Marshal(args)
		if op == extension.Wait {
			var w struct {
				Description string `json:"description"`
			}
			_ = json.Unmarshal(request.Input, &w)
			out = callOutcome{ended: op, summary: w.Description}
		}
	default:
		raw, _ := json.Marshal(args)
		request.Input, _ = json.Marshal(map[string]json.RawMessage{"data": raw})
	}
	result, err := tb.ToolbeltCall(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			// Cut off by an interrupt or stop: the host may have run it,
			// so it isn't answered as failed (finishCalls says so).
			return "", callOutcome{ended: cutOff}
		}
		return "error: " + toolError(err), callOutcome{}
	}
	var shown any
	switch op {
	case extension.ReadContext:
		shown = result.Context
		if result.Context == nil {
			shown = client.Payload{}
		}
	case extension.Emit:
		shown = result.Event
	case extension.CreateWork, extension.CancelWork:
		shown = result.Work
	case extension.Wait:
		shown = result.Wait
	case extension.DeclareOutcome:
		shown = map[string]string{"declared": "ok"}
	default:
		shown = map[string]any{}
		if result.Output != nil {
			if len(result.Output.Data) > 0 {
				shown = result.Output.Data
			} else {
				shown = result.Output
			}
		}
	}
	text, _ := json.Marshal(shown)
	return string(text), out
}

// toolError is what the model reads of a refused or failed call.
func toolError(err error) string {
	var api *client.Error
	switch {
	case errors.As(err, &api):
		return api.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer from the toolbelt in time; the operation may still finish"
	case errors.Is(err, client.ErrUnreachable):
		return "the toolbelt is unreachable"
	}
	return "the toolbelt call failed"
}
