package extensiontest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// shellEnv names the participant's directory when the test binary is run
// as the scripted shell participant (see init).
const shellEnv = "OPEN_CONDUIT_EXTENSIONTEST_SCRIPTED_SHELL"

// A test binary that imports this package is also the scripted shell
// participant: the script ScriptedShell writes runs it with shellEnv set,
// and it plays the next scripted turn and exits before any test runs.
func init() {
	if dir := os.Getenv(shellEnv); dir != "" {
		os.Exit(shellMain(dir, os.Stdout, os.Stderr))
	}
}

// Shell is a scripted participant for a shell executor binding. Path is
// the program to name in the binding's config.argv. It plays its turns in
// order, one per launch, making each call with the `os` CLI the shell
// executor puts on PATH, so give each Work a participant of its own; a
// launch past the last turn fails.
//
// A turn ends with its State as the attempt's outcome: completed, failed
// or handoff are declared with `os outcome`; waiting ends a turn whose
// calls included a wait, so the Work parks until the wait is satisfied
// and the next turn plays in the attempt that follows.
type Shell struct {
	Path string
	dir  string
}

// ScriptedShell writes a scripted shell participant into dir that plays
// turns.
func ScriptedShell(dir string, turns ...Turn) (*Shell, error) {
	path, dir, err := writeScript(dir, "participant", shellEnv, turns)
	if err != nil {
		return nil, err
	}
	return &Shell{Path: path, dir: dir}, nil
}

// Turn is the record of turn n (from 1), once that turn has made its calls.
func (s *Shell) Turn(n int) (TurnRecord, bool) { return readTurn(s.dir, n) }

// Turns are the records of the turns played so far.
func (s *Shell) Turns() []TurnRecord { return readTurns(s.dir) }

// writeScript writes turns.json and a script named name into dir that runs
// this test binary with env set to dir.
func writeScript(dir, name, env string, turns []Turn) (string, string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if turns == nil {
		turns = []Turn{}
	}
	b, err := json.Marshal(turns)
	if err != nil {
		return "", "", err
	}
	if err = os.WriteFile(filepath.Join(dir, "turns.json"), b, 0o600); err != nil {
		return "", "", err
	}
	script := fmt.Sprintf("#!/bin/sh\n%s=%s exec %s \"$@\"\n", env, shellQuote(dir), shellQuote(self))
	path := filepath.Join(dir, name)
	if err = os.WriteFile(path, []byte(script), 0o755); err != nil {
		return "", "", err
	}
	return path, dir, nil
}

// readTurn is the record of turn n a participant in dir wrote.
func readTurn(dir string, n int) (TurnRecord, bool) {
	var r TurnRecord
	b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("turn-%d.json", n)))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return TurnRecord{}, false
	}
	return r, true
}

func readTurns(dir string) []TurnRecord {
	var out []TurnRecord
	for n := 1; ; n++ {
		r, ok := readTurn(dir, n)
		if !ok {
			return out
		}
		out = append(out, r)
	}
}

// shellMain is one launch of the shell participant.
func shellMain(dir string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "scripted shell:", err)
		return 1
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
	if n > len(turns) {
		return fail(fmt.Errorf("launched for turn %d, but only %d are scripted", n, len(turns)))
	}
	turn := turns[n-1]
	switch turn.State {
	case "completed", "failed", "handoff", "waiting":
	default:
		return fail(fmt.Errorf("turn %d ends %q: a shell turn ends completed, failed, handoff or waiting", n, turn.State))
	}
	rec := TurnRecord{Args: os.Args[1:], Endpoint: os.Getenv("OS_ENDPOINT"), Results: []Result{}}
	if in := os.Getenv("OS_INPUT_FILE"); in != "" {
		if b, err := os.ReadFile(in); err == nil {
			rec.Input = string(b)
		}
	}
	if tok, err := os.ReadFile(os.Getenv("OS_TOKEN_FILE")); err == nil {
		rec.Token = strings.TrimSpace(string(tok))
	}
	for _, c := range turn.Calls {
		args, err := cliArgs(c)
		if err != nil {
			return fail(err)
		}
		r, err := runOS(args)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", c.Operation, err))
		}
		r.Operation, r.RequestID = c.Operation, c.RequestID
		rec.Results = append(rec.Results, r)
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
	if turn.State == "waiting" {
		return 0
	}
	summary := turn.Summary
	if summary == "" {
		summary = fmt.Sprintf("turn %d", n)
	}
	r, err := runOS([]string{"outcome", turn.State, "--summary", summary, "--request-id", fmt.Sprintf("turn%d-outcome", n)})
	if err != nil {
		return fail(err)
	}
	if r.IsError {
		return fail(fmt.Errorf("os outcome %s: %s", turn.State, r.Text))
	}
	fmt.Fprintln(stdout, r.Text)
	return 0
}

// runOS runs the `os` CLI on PATH: its output and success, or its error
// output and a refusal (exit 1). A usage error (exit 2) fails the turn, as
// the script asked for something the CLI does not take.
func runOS(args []string) (Result, error) {
	var out, errOut bytes.Buffer
	cmd := exec.Command("os", args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return Result{Text: strings.TrimSpace(out.String())}, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return Result{Text: strings.TrimSpace(errOut.String()), IsError: true}, nil
	case errors.As(err, &exit):
		return Result{}, fmt.Errorf("os %s: %s", args[0], strings.TrimSpace(errOut.String()))
	}
	return Result{}, err
}

// cliArgs is the `os` command that makes call: a core operation through
// its own command, any other through `os call` with the input as data.
func cliArgs(c Call) ([]string, error) {
	var in map[string]json.RawMessage
	if c.Input != nil {
		b, err := json.Marshal(c.Input)
		if err == nil {
			err = json.Unmarshal(b, &in)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: the input is not an object", c.Operation)
		}
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(in[k], &s)
		return s
	}
	keyFlags := func(raw json.RawMessage) []string {
		var keys []struct {
			Source     string `json:"source"`
			ExternalID string `json:"external_id"`
		}
		var one struct {
			Source     string `json:"source"`
			ExternalID string `json:"external_id"`
		}
		if json.Unmarshal(raw, &keys) != nil && json.Unmarshal(raw, &one) == nil && one.Source != "" {
			keys = append(keys, one)
		}
		var out []string
		for _, k := range keys {
			out = append(out, "--key-source", k.Source, "--key-external-id", k.ExternalID)
		}
		return out
	}
	// payload is a {schema, data} payload as --schema and --data.
	payload := func(raw json.RawMessage) []string {
		var p struct {
			Schema string          `json:"schema"`
			Data   json.RawMessage `json:"data"`
		}
		if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
			return nil
		}
		var out []string
		if p.Schema != "" {
			out = append(out, "--schema", p.Schema)
		}
		if len(p.Data) > 0 {
			out = append(out, "--data", string(p.Data))
		}
		return out
	}
	var args []string
	switch c.Operation {
	case "read_context":
		args = []string{"context"}
	case "emit":
		args = append([]string{"emit", "--type", str("type"), "--delivery-id", str("delivery_id")}, payload(in["data"])...)
		args = append(args, keyFlags(in["correlation_key"])...)
		if at := str("occurred_at"); at != "" {
			args = append(args, "--occurred-at", at)
		}
	case "create_work":
		args = []string{"work", "create", "--kind", str("kind")}
		if id := str("id"); id != "" {
			args = append(args, "--id", id)
		}
		args = append(append(args, keyFlags(in["correlation_keys"])...), payload(in["context"])...)
	case "wait":
		var match struct {
			Types []string        `json:"types"`
			Keys  json.RawMessage `json:"keys"`
		}
		_ = json.Unmarshal(in["match"], &match)
		args = []string{"wait"}
		if len(match.Types) > 0 {
			args = append(args, "--type", strings.Join(match.Types, ","))
		}
		args = append(args, keyFlags(match.Keys)...)
		var timeout int64
		if json.Unmarshal(in["timeout_seconds"], &timeout) == nil && timeout > 0 {
			args = append(args, "--timeout", strconv.FormatInt(timeout, 10))
		}
		if s := str("after_sequence"); s != "" {
			args = append(args, "--after-sequence", s)
		}
		if s := str("description"); s != "" {
			args = append(args, "--description", s)
		}
	case "declare_outcome":
		args = []string{"outcome", str("kind")}
		if s := str("summary"); s != "" {
			args = append(args, "--summary", s)
		}
		args = append(args, payload(in["data"])...)
	default:
		args = []string{"call", c.Operation}
		if c.Input != nil {
			b, err := json.Marshal(c.Input)
			if err != nil {
				return nil, err
			}
			args = append(args, "--data", string(b))
		}
	}
	if c.RequestID != "" {
		args = append(args, "--request-id", c.RequestID)
	}
	return args, nil
}
