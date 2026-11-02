package extensiontest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFakeCodexTurns plays a fresh turn and a resumed one without a
// toolbelt: each records its input and arguments, starts or continues
// the thread and writes its status where --output-last-message says, and
// a launch past the last turn fails.
func TestFakeCodexTurns(t *testing.T) {
	dir := t.TempDir()
	c, err := FakeCodex(dir, Turn{State: "help", Summary: "asked"}, Turn{State: "finished"})
	if err != nil {
		t.Fatal(err)
	}
	if script, _ := os.ReadFile(c.Path); !strings.Contains(string(script), fakeCodexEnv+"=") {
		t.Fatalf("script: %s", script)
	}
	last := filepath.Join(dir, "last.json")
	for n, tc := range []struct {
		args         []string
		input, state string
		thread       bool
	}{
		{[]string{"exec", "--json", "--output-last-message", last, "-"}, "start here", "help", true},
		{[]string{"exec", "resume", "--json", "--output-last-message", last, "fake-thread", "-"}, "you were answered", "finished", false},
	} {
		var out, errOut bytes.Buffer
		if code := fakeCodexMain(dir, tc.args, strings.NewReader(tc.input), &out, &errOut); code != 0 {
			t.Fatalf("turn %d: %d %s", n+1, code, errOut.String())
		}
		if got := strings.Contains(out.String(), `"thread_id":"fake-thread"`); got != tc.thread {
			t.Fatalf("turn %d thread.started %v: %s", n+1, got, out.String())
		}
		var status map[string]string
		b, _ := os.ReadFile(last)
		if err := json.Unmarshal(b, &status); err != nil || status["state"] != tc.state {
			t.Fatalf("turn %d status: %s %v", n+1, b, err)
		}
	}
	turns := c.Turns()
	if len(turns) != 2 || turns[0].Input != "start here" || turns[1].Input != "you were answered" || turns[1].Args[1] != "resume" {
		t.Fatalf("records: %+v", turns)
	}
	var out, errOut bytes.Buffer
	if code := fakeCodexMain(dir, []string{"exec", "--output-last-message", last, "-"}, strings.NewReader("again"), &out, &errOut); code == 0 {
		t.Fatal("a launch past the last turn succeeded")
	}
}

// TestCodexMCPOverrides reads the toolbelt server back from the overrides
// the codex executor writes.
func TestCodexMCPOverrides(t *testing.T) {
	s, ok, err := codexMCP([]string{"exec", "-c", `sandbox_mode="danger-full-access"`,
		"-c", `mcp_servers.open-conduit.command="/opt/oc/os"`, "-c", `mcp_servers.open-conduit.args=["mcp"]`,
		"-c", `mcp_servers.open-conduit.env={OS_ENDPOINT="unix:///tmp/a \"b\".sock",OS_TOKEN_FILE="/tmp/token"}`})
	if err != nil || !ok || s.Command != "/opt/oc/os" || len(s.Args) != 1 || s.Args[0] != "mcp" ||
		s.Env["OS_ENDPOINT"] != `unix:///tmp/a "b".sock` || s.Env["OS_TOKEN_FILE"] != "/tmp/token" {
		t.Fatalf("%+v %v %v", s, ok, err)
	}
	if _, ok, _ := codexMCP([]string{"exec", "-"}); ok {
		t.Fatal("found a server in no overrides")
	}
}
