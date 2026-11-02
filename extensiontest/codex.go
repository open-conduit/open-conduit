package extensiontest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// fakeCodexEnv names the fake's directory when the test binary is run as
// the fake Codex CLI (see init).
const fakeCodexEnv = "OPEN_CONDUIT_EXTENSIONTEST_FAKE_CODEX"

func init() {
	if dir := os.Getenv(fakeCodexEnv); dir != "" {
		os.Exit(fakeCodexMain(dir, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
}

// Codex is a fake Codex CLI. Path is the binary to name in a codex
// binding's config.binary. Like Claude, it plays its turns in order, one
// per launch (a turn and any follow-up turn for injected input are two
// launches), so give each Work a fake of its own.
type Codex struct {
	Path string
	dir  string
}

// FakeCodex writes a fake Codex CLI into dir that plays turns. It speaks
// `codex exec --json` as the codex executor drives it: a fresh launch
// starts thread fake-thread and a `resume` launch continues it, each call
// goes through the `os mcp` server the -c mcp_servers.open-conduit.*
// overrides name, and each turn ends with its State as the structured
// status, written to --output-last-message.
func FakeCodex(dir string, turns ...Turn) (*Codex, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b, err := json.Marshal(turns)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "turns.json"), b, 0o600); err != nil {
		return nil, err
	}
	script := fmt.Sprintf("#!/bin/sh\n%s=%s exec %s \"$@\"\n", fakeCodexEnv, shellQuote(dir), shellQuote(self))
	path := filepath.Join(dir, "codex")
	if err = os.WriteFile(path, []byte(script), 0o755); err != nil {
		return nil, err
	}
	return &Codex{Path: path, dir: dir}, nil
}

// Turn is the record of turn n (from 1), once that turn has made its calls.
func (c *Codex) Turn(n int) (TurnRecord, bool) { return (&Claude{dir: c.dir}).Turn(n) }

// Turns are the records of the turns played so far.
func (c *Codex) Turns() []TurnRecord { return (&Claude{dir: c.dir}).Turns() }

var tomlPair = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)=("(?:[^"\\]|\\.)*")`)

// codexMCP reads the toolbelt server from the -c overrides.
func codexMCP(args []string) (mcpServer, bool, error) {
	var s mcpServer
	found := false
	prefix := "mcp_servers.open-conduit."
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" || !strings.HasPrefix(args[i+1], prefix) {
			continue
		}
		key, value, _ := strings.Cut(strings.TrimPrefix(args[i+1], prefix), "=")
		found = true
		var err error
		switch key {
		case "command":
			err = json.Unmarshal([]byte(value), &s.Command)
		case "args":
			err = json.Unmarshal([]byte(value), &s.Args)
		case "env":
			s.Env = map[string]string{}
			for _, m := range tomlPair.FindAllStringSubmatch(value, -1) {
				var v string
				if err = json.Unmarshal([]byte(m[2]), &v); err != nil {
					break
				}
				s.Env[m[1]] = v
			}
		}
		if err != nil {
			return s, true, fmt.Errorf("the %s override is not TOML the fake reads: %v", key, err)
		}
	}
	return s, found, nil
}

// fakeCodexMain is one launch of the fake.
func fakeCodexMain(dir string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Fprintln(stdout, "codex-cli 9.9.9")
		return 0
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "fake codex:", err)
		return 1
	}
	resume, last := false, ""
	for i, a := range args {
		switch a {
		case "resume":
			resume = true
		case "--output-last-message":
			if i+1 < len(args) {
				last = args[i+1]
			}
		}
	}
	if len(args) == 0 || args[0] != "exec" || last == "" {
		return fail(errors.New("not run as codex exec --output-last-message FILE"))
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
	input, err := io.ReadAll(stdin)
	if err != nil {
		return fail(err)
	}
	if n > len(turns) {
		return fail(fmt.Errorf("launched for turn %d, but only %d are scripted", n, len(turns)))
	}
	out := json.NewEncoder(stdout)
	if !resume {
		_ = out.Encode(map[string]any{"type": "thread.started", "thread_id": "fake-thread"})
	}
	_ = out.Encode(map[string]any{"type": "turn.started"})
	turn := turns[n-1]
	rec := TurnRecord{Args: args, Input: string(input), Results: []Result{}}
	server, ok, err := codexMCP(args)
	if err != nil {
		return fail(err)
	}
	if ok {
		rec.Endpoint = server.Env["OS_ENDPOINT"]
		if tok, err := os.ReadFile(server.Env["OS_TOKEN_FILE"]); err == nil {
			rec.Token = strings.TrimSpace(string(tok))
		}
		if len(turn.Calls) > 0 {
			if rec.Results, err = server.calls(turn.Calls); err != nil {
				return fail(err)
			}
		}
		for range rec.Results {
			_ = out.Encode(map[string]any{"type": "item.completed", "item": map[string]any{"type": "mcp_tool_call"}})
		}
	} else if len(turn.Calls) > 0 {
		return fail(errors.New("the turn makes toolbelt calls but codex was given no open-conduit MCP server"))
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
	summary := turn.Summary
	if summary == "" {
		summary = fmt.Sprintf("turn %d", n)
	}
	_ = out.Encode(map[string]any{"type": "turn.completed", "usage": map[string]int{"input_tokens": 1, "cached_input_tokens": 0, "output_tokens": 1}})
	status, _ := json.Marshal(map[string]string{"state": turn.State, "summary": summary, "handoff": "", "report": ""})
	if err = os.WriteFile(last, status, 0o600); err != nil {
		return fail(err)
	}
	return 0
}
