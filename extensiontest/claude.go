package extensiontest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// fakeEnv names the fake's directory when the test binary is run as the
// fake Claude Code CLI (see init).
const fakeEnv = "OPEN_CONDUIT_EXTENSIONTEST_FAKE_CLAUDE"

// A test binary that imports this package is also the fake Claude Code CLI:
// the script FakeClaude writes runs it with fakeEnv set, and it acts out the
// next scripted turn and exits before any test runs.
func init() {
	if dir := os.Getenv(fakeEnv); dir != "" {
		os.Exit(fakeMain(dir, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
}

// Call is one toolbelt call a turn makes. Operation is a core operation
// (read_context, emit, create_work, wait, declare_outcome) or one an
// extension registered; Input is its input object.
type Call struct {
	Operation string `json:"operation"`
	Input     any    `json:"input,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// Turn is one scripted turn: its calls in order, then, when Barrier is set,
// a wait until that file exists (so the test can act while the turn is
// still running), then the end of the turn with State. For ScriptedShell,
// State is the attempt's outcome (completed, failed, handoff, or waiting
// after a wait call); for FakeClaude it is the structured status the turn
// ends with (whatever the binding's status enum allows).
type Turn struct {
	Calls   []Call `json:"calls,omitempty"`
	Barrier string `json:"barrier,omitempty"`
	State   string `json:"state"`
	Summary string `json:"summary,omitempty"`
}

// Result is what one call returned through the toolbelt: the result's text
// (the JSON the `os` CLI or its MCP server prints; the error text for a
// refusal) and whether it was a refusal.
type Result struct {
	Operation string `json:"operation"`
	RequestID string `json:"request_id,omitempty"`
	Text      string `json:"text"`
	IsError   bool   `json:"is_error"`
}

// TurnRecord is what a participant saw in one turn: its arguments, its
// input (the message FakeClaude was started or resumed with; the content of
// OS_INPUT_FILE for ScriptedShell), its toolbelt endpoint and token, and its
// calls' results. It is written once the calls are made, before the
// barrier.
type TurnRecord struct {
	Args     []string `json:"args"`
	Input    string   `json:"input"`
	Endpoint string   `json:"endpoint,omitempty"`
	Token    string   `json:"token,omitempty"`
	Results  []Result `json:"results"`
}

// Claude is a fake Claude Code CLI. Path is the binary to name in a
// claude-code binding's config.binary. It plays its turns in order, one per
// launch, so give each Work a fake of its own; a launch past the last turn
// fails.
type Claude struct {
	Path string
	dir  string
}

// FakeClaude writes a fake Claude Code CLI into dir that plays turns. It
// speaks stream-json as the claude-code executor drives it, makes each
// call through the `os mcp` server named in its --mcp-config, writes the
// session transcript under CLAUDE_CONFIG_DIR (so a later turn can resume)
// and ends each turn with a structured status.
func FakeClaude(dir string, turns ...Turn) (*Claude, error) {
	path, dir, err := writeScript(dir, "claude", fakeEnv, turns)
	if err != nil {
		return nil, err
	}
	return &Claude{Path: path, dir: dir}, nil
}

// Turn is the record of turn n (from 1), once that turn has made its calls.
func (c *Claude) Turn(n int) (TurnRecord, bool) { return readTurn(c.dir, n) }

// Turns are the records of the turns played so far.
func (c *Claude) Turns() []TurnRecord { return readTurns(c.dir) }

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// fakeMain is one launch of the fake.
func fakeMain(dir string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Fprintln(stdout, "9.9.9 (Claude Code)")
		return 0
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "fake claude:", err)
		return 1
	}
	var session, mcp string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--session-id", "--resume":
			session = args[i+1]
		case "--mcp-config":
			mcp = args[i+1]
		}
	}
	var turns []Turn
	b, err := os.ReadFile(filepath.Join(dir, "turns.json"))
	if err != nil {
		return fail(err)
	}
	if err = json.Unmarshal(b, &turns); err != nil {
		return fail(err)
	}
	n, err := claimTurn(dir)
	if err != nil {
		return fail(err)
	}
	out := json.NewEncoder(stdout)
	_ = out.Encode(map[string]any{"type": "system", "subtype": "init"})
	// The first user message is the turn's input; claude echoes each one
	// back (--replay-user-messages), which is how the executor knows an
	// input was taken in.
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return fail(fmt.Errorf("no input: %v", err))
	}
	var msg struct {
		Message struct {
			Content any `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal([]byte(line), &msg)
	input, _ := msg.Message.Content.(string)
	_ = out.Encode(map[string]any{"type": "user", "isReplay": true, "message": map[string]any{"role": "user", "content": msg.Message.Content}})
	if n > len(turns) {
		return fail(fmt.Errorf("launched for turn %d, but only %d are scripted", n, len(turns)))
	}
	turn := turns[n-1]
	rec := TurnRecord{Args: args, Input: input, Results: []Result{}}
	if mcp != "" {
		server, err := readMCPConfig(mcp)
		if err != nil {
			return fail(err)
		}
		rec.Endpoint = server.Env["OS_ENDPOINT"]
		if tok, err := os.ReadFile(server.Env["OS_TOKEN_FILE"]); err == nil {
			rec.Token = strings.TrimSpace(string(tok))
		}
		if len(turn.Calls) > 0 {
			if rec.Results, err = server.calls(turn.Calls); err != nil {
				return fail(err)
			}
		}
	} else if len(turn.Calls) > 0 {
		return fail(errors.New("the turn makes toolbelt calls but claude was given no --mcp-config"))
	}
	if err = writeRecord(filepath.Join(dir, fmt.Sprintf("turn-%d.json", n)), rec); err != nil {
		return fail(err)
	}
	for turn.Barrier != "" {
		if _, err := os.Stat(turn.Barrier); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = appendTranscript(session); err != nil {
		return fail(err)
	}
	summary := turn.Summary
	if summary == "" {
		summary = fmt.Sprintf("turn %d", n)
	}
	_ = out.Encode(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "ok", "session_id": session, "num_turns": 1, "total_cost_usd": 0,
		"structured_output": map[string]any{"state": turn.State, "summary": summary, "handoff": "none"}})
	return 0
}

// claimTurn takes the next turn number; launches are sequential for one
// Work, and the exclusive create keeps two from ever sharing one.
func claimTurn(dir string) (int, error) {
	for n := 1; ; n++ {
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("turn-%d.claimed", n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return n, f.Close()
	}
}

func writeRecord(path string, rec TurnRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// appendTranscript adds a line to the session's transcript where claude
// keeps it, which is what a resume looks for.
func appendTranscript(session string) error {
	if session == "" {
		return nil
	}
	config := os.Getenv("CLAUDE_CONFIG_DIR")
	if config == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		config = filepath.Join(home, ".claude")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	dir := filepath.Join(config, "projects", nonAlnum.ReplaceAllString(cwd, "-"))
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, session+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(`{"type":"user"}` + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

type mcpServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// readMCPConfig is the one server an --mcp-config names.
func readMCPConfig(path string) (mcpServer, error) {
	var doc struct {
		Servers map[string]mcpServer `json:"mcpServers"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return mcpServer{}, err
	}
	if err = json.Unmarshal(b, &doc); err != nil {
		return mcpServer{}, err
	}
	for _, s := range doc.Servers {
		return s, nil
	}
	return mcpServer{}, errors.New("the MCP config names no server")
}

// coreTools are the toolbelt operations `os mcp` offers as tools of their
// own; any other operation goes through its `call` tool.
var coreTools = map[string]bool{"read_context": true, "emit": true, "create_work": true, "wait": true, "declare_outcome": true}

// calls starts the server, initializes it and makes each call in turn.
func (s mcpServer) calls(calls []Call) ([]Result, error) {
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = w.Close(); _ = cmd.Wait() }()
	in := bufio.NewScanner(r)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(w)
	id := 0
	request := func(method string, params any) (json.RawMessage, error) {
		id++
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for in.Scan() {
			var resp struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(in.Bytes(), &resp) != nil || resp.ID != id {
				continue
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
			}
			return resp.Result, nil
		}
		return nil, fmt.Errorf("%s: the MCP server closed its output: %v", method, in.Err())
	}
	if _, err = request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "extensiontest", "version": "0"}}); err != nil {
		return nil, err
	}
	if err = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return nil, err
	}
	out := []Result{}
	for _, c := range calls {
		name, arguments := "call", map[string]any{"operation": c.Operation}
		if c.Input != nil {
			arguments["input"] = c.Input
		}
		if coreTools[c.Operation] {
			name, arguments = c.Operation, map[string]any{}
			if c.Input != nil {
				b, err := json.Marshal(c.Input)
				if err != nil {
					return nil, err
				}
				if err = json.Unmarshal(b, &arguments); err != nil {
					return nil, fmt.Errorf("%s: the input is not an object", c.Operation)
				}
			}
		}
		if c.RequestID != "" {
			arguments["request_id"] = c.RequestID
		}
		raw, err := request("tools/call", map[string]any{"name": name, "arguments": arguments})
		if err != nil {
			return nil, err
		}
		var res struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		if err = json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		text := ""
		for _, part := range res.Content {
			text += part.Text
		}
		out = append(out, Result{Operation: c.Operation, RequestID: c.RequestID, Text: text, IsError: res.IsError})
	}
	return out, nil
}
