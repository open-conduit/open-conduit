package extensiontest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFakeClaudeTurns plays two turns without a toolbelt: each echoes its
// input as a replay, records it and ends with its state, the transcript a
// resume looks for is written, and a launch past the last turn fails.
func TestFakeClaudeTurns(t *testing.T) {
	dir := t.TempDir()
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	c, err := FakeClaude(dir, Turn{State: "help", Summary: "asked"}, Turn{State: "finished"})
	if err != nil {
		t.Fatal(err)
	}
	if script, _ := os.ReadFile(c.Path); !strings.Contains(string(script), fakeEnv+"=") {
		t.Fatalf("script: %s", script)
	}
	launch := func(args []string, input string) (int, []map[string]any) {
		t.Helper()
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": input}})
		var out, errOut bytes.Buffer
		code := fakeMain(dir, args, bytes.NewReader(append(line, '\n')), &out, &errOut)
		var msgs []map[string]any
		for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("not stream-json: %q", l)
			}
			msgs = append(msgs, m)
		}
		return code, msgs
	}
	for n, tc := range []struct {
		args         []string
		input, state string
	}{
		{[]string{"-p", "--session-id", "s1"}, "start \"here\"", "help"},
		{[]string{"-p", "--resume", "s1"}, "you were answered", "finished"},
	} {
		code, msgs := launch(tc.args, tc.input)
		if code != 0 || len(msgs) != 3 || msgs[0]["type"] != "system" || msgs[1]["isReplay"] != true || msgs[1]["message"].(map[string]any)["content"] != tc.input {
			t.Fatalf("turn %d: %d %v", n+1, code, msgs)
		}
		status := msgs[2]["structured_output"].(map[string]any)
		if msgs[2]["type"] != "result" || msgs[2]["session_id"] != "s1" || status["state"] != tc.state {
			t.Fatalf("turn %d result: %v", n+1, msgs[2])
		}
		if n == 0 && status["summary"] != "asked" {
			t.Fatalf("summary: %v", status)
		}
	}
	turns := c.Turns()
	if len(turns) != 2 || turns[0].Input != "start \"here\"" || turns[1].Input != "you were answered" || strings.Join(turns[1].Args, " ") != "-p --resume s1" {
		t.Fatalf("records: %+v", turns)
	}
	cwd, _ := os.Getwd()
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	transcript, err := os.ReadFile(filepath.Join(config, "projects", nonAlnum.ReplaceAllString(cwd, "-"), "s1.jsonl"))
	if err != nil || strings.Count(string(transcript), "\n") != 2 {
		t.Fatalf("transcript: %q %v", transcript, err)
	}
	if code, _ := launch([]string{"-p", "--resume", "s1"}, "again"); code == 0 {
		t.Fatal("a launch past the last turn succeeded")
	}
}

// TestAgentScript scripts turns fluently: calls join the open turn (each
// with a request ID of its own), Ends closes it, Ends with none open is a
// turn of no calls, and Turn starts the next.
func TestAgentScript(t *testing.T) {
	c, err := FakeClaude(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{d: &Instance{t: t}, c: c}
	a.Call("weather.forecast", map[string]any{"city": "Leeds"}).Call("weather.report", nil).Ends("help").
		Ends("review").
		Turn().Call("read_context", nil).Ends("finished")
	var turns []Turn
	b, _ := os.ReadFile(filepath.Join(c.dir, "turns.json"))
	if err := json.Unmarshal(b, &turns); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(turns)
	want := `[{"calls":[{"operation":"weather.forecast","input":{"city":"Leeds"},"request_id":"turn1-call1"},{"operation":"weather.report","request_id":"turn1-call2"}],"state":"help"},{"state":"review"},{"calls":[{"operation":"read_context","request_id":"turn3-call1"}],"state":"finished"}]`
	if string(got) != want {
		t.Fatalf("turns:\n%s\nwant\n%s", got, want)
	}
}

// TestName makes config identifiers of declared names.
func TestName(t *testing.T) {
	for in, want := range map[string]string{"notes": "notes", "acme.Board": "acme-board", "9lives": "x9lives", "a_b-c": "a_b-c"} {
		if got := name(in); got != want {
			t.Errorf("name(%q) = %q, want %q", in, got, want)
		}
	}
}
