package extensiontest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentScript scripts turns fluently: calls join the open turn (each
// with a request ID of its own), Ends closes it, Ends with none open is a
// turn of no calls, and Turn starts the next.
func TestAgentScript(t *testing.T) {
	s, err := ScriptedShell(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{d: &Instance{t: t}, dir: s.dir}
	a.Call("weather.forecast", map[string]any{"city": "Leeds"}).Call("weather.report", nil).Ends("waiting").
		Ends("handoff").
		Turn().Call("read_context", nil).Ends("completed")
	var turns []Turn
	b, _ := os.ReadFile(filepath.Join(s.dir, "turns.json"))
	if err := json.Unmarshal(b, &turns); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(turns)
	want := `[{"calls":[{"operation":"weather.forecast","input":{"city":"Leeds"},"request_id":"turn1-call1"},{"operation":"weather.report","request_id":"turn1-call2"}],"state":"waiting"},{"state":"handoff"},{"calls":[{"operation":"read_context","request_id":"turn3-call1"}],"state":"completed"}]`
	if string(got) != want {
		t.Fatalf("turns:\n%s\nwant\n%s", got, want)
	}
}

// TestCLIArgs maps each call to the `os` command that makes it: core
// operations through their own commands, any other through `os call`.
func TestCLIArgs(t *testing.T) {
	for _, tc := range []struct {
		call Call
		want string
	}{
		{Call{Operation: "weather.forecast", Input: map[string]any{"city": "Leeds"}, RequestID: "r1"}, `call weather.forecast --data {"city":"Leeds"} --request-id r1`},
		{Call{Operation: "weather.report"}, `call weather.report`},
		{Call{Operation: "read_context"}, `context`},
		{Call{Operation: "wait", Input: map[string]any{"match": map[string]any{"types": []string{"a", "b"}, "keys": []map[string]string{{"source": "s", "external_id": "x"}}}, "timeout_seconds": 60, "description": "d"}}, `wait --type a,b --key-source s --key-external-id x --timeout 60 --description d`},
		{Call{Operation: "emit", Input: map[string]any{"type": "t", "delivery_id": "d1", "data": map[string]any{"schema": "s.v1", "data": map[string]any{"n": 1}}, "correlation_key": map[string]string{"source": "s", "external_id": "x"}}}, `emit --type t --delivery-id d1 --schema s.v1 --data {"n":1} --key-source s --key-external-id x`},
		{Call{Operation: "create_work", Input: map[string]any{"kind": "k", "id": "w1", "context": map[string]any{"data": map[string]any{"n": 1}}}}, `work create --kind k --id w1 --data {"n":1}`},
		{Call{Operation: "declare_outcome", Input: map[string]any{"kind": "failed", "summary": "no"}}, `outcome failed --summary no`},
	} {
		args, err := cliArgs(tc.call)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(args, " "); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.call.Operation, got, tc.want)
		}
	}
	if _, err := cliArgs(Call{Operation: "x", Input: []int{1}}); err == nil {
		t.Fatal("an input that is not an object was taken")
	}
}

// TestScriptedShellTurns plays turns against a stand-in `os` on PATH: each
// call's output is its result, an exit 1 a refusal, the turn's state is
// declared with `os outcome` (but not for waiting), an unknown state and a
// launch past the last turn fail.
func TestScriptedShellTurns(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	// The stand-in prints its arguments, refusing `call bad`.
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(log) + "\nif [ \"$2\" = bad ]; then echo 'os: 400 invalid: no' >&2; exit 1; fi\necho \"{\\\"args\\\": \\\"$*\\\"}\"\n"
	if err := os.WriteFile(filepath.Join(bin, "os"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	input := filepath.Join(bin, "input.json")
	if err := os.WriteFile(input, []byte(`{"trigger":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OS_INPUT_FILE", input)
	t.Setenv("OS_ENDPOINT", "unix:///x")
	s, err := ScriptedShell(t.TempDir(),
		Turn{Calls: []Call{{Operation: "good", Input: map[string]any{"a": 1}}, {Operation: "bad"}}, State: "completed", Summary: "done"},
		Turn{Calls: []Call{{Operation: "wait", Input: map[string]any{"match": map[string]any{"types": []string{"e"}}}}}, State: "waiting"},
		Turn{State: "finished"})
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := shellMain(s.dir, &out, &errOut); code != 0 {
		t.Fatalf("turn 1: %d %s", code, errOut.String())
	}
	r, ok := s.Turn(1)
	if !ok || r.Input != `{"trigger":{}}` || r.Endpoint != "unix:///x" || len(r.Results) != 2 {
		t.Fatalf("turn 1 record: %+v", r)
	}
	if r.Results[0].IsError || !strings.Contains(r.Results[0].Text, `call good --data {"a":1}`) || r.Results[0].Operation != "good" {
		t.Fatalf("good: %+v", r.Results[0])
	}
	if !r.Results[1].IsError || r.Results[1].Text != "os: 400 invalid: no" {
		t.Fatalf("bad: %+v", r.Results[1])
	}
	if code := shellMain(s.dir, &out, &errOut); code != 0 {
		t.Fatalf("turn 2: %d %s", code, errOut.String())
	}
	calls, _ := os.ReadFile(log)
	want := "call good --data {\"a\":1}\ncall bad\noutcome completed --summary done --request-id turn1-outcome\nwait --type e\n"
	if string(calls) != want {
		t.Fatalf("os calls:\n%s\nwant\n%s", calls, want)
	}
	if code := shellMain(s.dir, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "completed, failed, handoff or waiting") {
		t.Fatalf("a work-issue state was taken: %d %s", code, errOut.String())
	}
	errOut.Reset()
	if code := shellMain(s.dir, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "only 3 are scripted") {
		t.Fatalf("a launch past the last turn: %d %s", code, errOut.String())
	}
}
