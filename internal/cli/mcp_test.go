package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// os mcp's initialize answers instructions made of the base sentence, the
// granted operations' summaries and inputs and the guidance the toolbelt
// serves for them; when the toolbelt answers none, or not at all, the base
// sentence alone.
func TestMCPInstructionsCarryGuidance(t *testing.T) {
	dir, err := os.MkdirTemp("", "ocmcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	answer := `{"data":{"guidance":["Note things with fixture.note.","Ask with fixture.ask."]}}`
	status := http.StatusOK
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" || r.URL.Path != "/v1/toolbelt/guidance" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"unknown endpoint"}}`)
			return
		}
		w.WriteHeader(status)
		fmt.Fprint(w, answer)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	env := Env{Getenv: func(k string) string {
		switch k {
		case "OS_ENDPOINT":
			return "unix://" + socket
		case "OS_TOKEN":
			return "tok"
		}
		return ""
	}}
	instructions := func() string {
		t.Helper()
		var out, errOut bytes.Buffer
		in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
		if code := RunToolbelt("t", "c", []string{"mcp"}, env, strings.NewReader(in), &out, &errOut); code != 0 {
			t.Fatalf("mcp exited %d: %s", code, errOut.String())
		}
		var resp struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("response %q: %v", out.String(), err)
		}
		return resp.Result.Instructions
	}
	want := mcpBaseInstructions + "\n\nNote things with fixture.note.\n\nAsk with fixture.ask."
	if got := instructions(); got != want {
		t.Fatalf("instructions = %q", got)
	}
	// The granted operations come first, each with its summary and input
	// fields, required ones first and optional ones marked with ?.
	answer = `{"data":{"guidance":["Ask with fixture.ask."],"operations":[` +
		`{"name":"fixture.note","summary":"Note a thing.","input":{"type":"object","properties":{"text":{"type":"string"},"tag":{"type":"string"},"body":{"type":"string"}},"required":["text"]}},` +
		`{"name":"fixture.ping","summary":"Answer pong.","input":{"type":"object","properties":{}}}]}}`
	want = mcpBaseInstructions + "\n\n## Operations\n\nCall these with the `call` tool, giving the `operation` and its `input` object:\n" +
		"\n- `fixture.note`: Note a thing. Input: `text`, `body`?, `tag`?.\n- `fixture.ping`: Answer pong.\n\nAsk with fixture.ask."
	if got := instructions(); got != want {
		t.Fatalf("instructions with operations = %q", got)
	}
	answer = `{"data":{"guidance":[]}}`
	if got := instructions(); got != mcpBaseInstructions {
		t.Fatalf("instructions without guidance = %q", got)
	}
	status, answer = http.StatusNotFound, `{"error":{"code":"not_found","message":"unknown endpoint"}}`
	if got := instructions(); got != mcpBaseInstructions {
		t.Fatalf("instructions from an older daemon = %q", got)
	}
}

// The call tool naming a builtin (read_context, emit, …) does what the
// builtin's own tool does: the same request reaches the host, and the result
// shows the builtin's part of it (read_context's context), never call's empty
// output. An extension operation through call still shows its output
// (control), and os call refuses a builtin, naming its command.
func TestMCPCallOfBuiltinActsAsItsTool(t *testing.T) {
	dir, err := os.MkdirTemp("", "ocmcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var requests []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		delete(body, "scope_id")
		delete(body, "work_id")
		delete(body, "execution_id")
		b, _ := json.Marshal(body)
		requests = append(requests, string(b))
		var op string
		_ = json.Unmarshal(body["operation"], &op)
		switch op {
		case "read_context":
			fmt.Fprint(w, `{"data":{"context":{"schema":"playground.chat.v1","data":{"message":"Reply STILL."}}}}`)
		case "emit":
			fmt.Fprint(w, `{"data":{"event":{"id":"ev1","type":"agent.note"}}}`)
		default:
			fmt.Fprint(w, `{"data":{"output":{"schema":"fixture.v1","data":{"pong":true}}}}`)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	env := Env{Getenv: func(k string) string {
		switch k {
		case "OS_ENDPOINT":
			return "unix://" + socket
		case "OS_TOKEN":
			return "tok"
		}
		return ""
	}}
	tb := &toolbelt{env: env}
	shown := func(name, arguments string) string {
		t.Helper()
		res := tb.mcpCall(name, json.RawMessage(arguments))
		text := res["content"].([]map[string]any)[0]["text"].(string)
		if res["isError"] != false {
			t.Fatalf("%s %s failed: %s", name, arguments, text)
		}
		var v any
		_ = json.Unmarshal([]byte(text), &v)
		b, _ := json.Marshal(v)
		return string(b)
	}
	const context = `{"data":{"message":"Reply STILL."},"schema":"playground.chat.v1"}`
	if got := shown("read_context", `{}`); got != context {
		t.Fatalf("read_context shows %s", got)
	}
	for _, args := range []string{`{"operation":"read_context"}`, `{"operation":"read_context","input":{}}`} {
		if got := shown("call", args); got != context {
			t.Fatalf("call %s shows %s, want the context", args, got)
		}
	}
	requests = nil
	emit := `{"type":"agent.note","data":{"schema":"note.v1","data":{"n":1}}}`
	ownTool := shown("emit", `{"type":"agent.note","data":{"schema":"note.v1","data":{"n":1}},"request_id":"r1"}`)
	viaCall := shown("call", `{"operation":"emit","input":`+emit+`,"request_id":"r1"}`)
	if ownTool != viaCall || !strings.Contains(viaCall, `"ev1"`) {
		t.Fatalf("emit shows %s, call emit shows %s", ownTool, viaCall)
	}
	if len(requests) != 2 || requests[0] != requests[1] {
		t.Fatalf("emit and call emit sent %v, want the same request", requests)
	}
	if got := shown("call", `{"operation":"fixture.ping","input":{}}`); got != `{"data":{"pong":true},"schema":"fixture.v1"}` {
		t.Fatalf("call fixture.ping shows %s", got)
	}
	var out, errOut bytes.Buffer
	if code := RunToolbelt("t", "c", []string{"call", "read_context"}, env, strings.NewReader(""), &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "use os context") {
		t.Fatalf("os call read_context exited %d: %q %q", code, out.String(), errOut.String())
	}
}
