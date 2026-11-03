package claudecodeext

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json: a fixture command
// that writes a fake Claude Code CLI into the runner's directory and names
// it as the binding's binary, so certification never runs the real one.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(Name), "certify-fixture")}
}

// fakeClaude is a Claude Code CLI as the executor drives it: stream-json
// in and out. It echoes FAKE_MESSAGES user messages back as replays (1 by
// default; the inject case's second is the injected Event, so the replay
// is its consumption proof), writes the session transcript where claude
// keeps it, and ends with a structured status of review.
const fakeClaude = `#!/bin/sh
[ "$1" = --version ] && { echo "9.9.9 (Claude Code)"; exit 0; }
session=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session-id|--resume) session=$2; shift ;;
  esac
  shift
done
t="$CLAUDE_CONFIG_DIR/projects/$(pwd -P | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$t"
echo '{"type":"system","subtype":"init"}'
want=${FAKE_MESSAGES:-1}
n=0
while IFS= read -r line; do
  n=$((n+1))
  content=$(printf '%s' "$line" | sed -E 's/^\{"message":\{"content":"(.*)","role":"user"\},"type":"user"\}$/\1/')
  printf '{"type":"user","isReplay":true,"message":{"role":"user","content":"%s"}}\n' "$content"
  [ "$n" -ge "$want" ] && break
done
echo '{"type":"user","timestamp":"2026-11-03T10:00:00Z"}' >> "$t/$session.jsonl"
printf '{"type":"result","subtype":"success","is_error":false,"result":"certified","session_id":"%s","num_turns":1,"total_cost_usd":0,"structured_output":{"state":"review","summary":"certified","handoff":"none"}}\n' "$session"
`

// Fixture is the certification fixture for the fake at binary, with
// Claude's config directory under {tmp}: one turn for the executor case,
// and for the inject case a turn that waits for the injected Event as its
// second message. Both run without the toolbelt.
func Fixture(binary string) protocol.Fixture {
	env := func(extra map[string]string) map[string]string {
		out := map[string]string{"CLAUDE_CONFIG_DIR": "{tmp}/claude-config"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	run, _ := json.Marshal(map[string]any{"binary": binary, "env": env(nil)})
	inject, _ := json.Marshal(map[string]any{"binary": binary, "env": env(map[string]string{"FAKE_MESSAGES": "2"})})
	return protocol.Fixture{
		Executor:      &protocol.ExecutorCase{Executor: Name, Config: protocol.Payload{Data: run}},
		Inject:        &protocol.InjectCase{Executor: Name, Config: protocol.Payload{Data: inject}, FinishFile: "{tmp}/inject-go"},
		ExecutorModes: []protocol.ExecutorModeCase{{Executor: Name, Config: json.RawMessage(`{}`), Mode: "work"}, {Executor: Name, Config: json.RawMessage(`{"workspace_mode":"clone"}`), Mode: "clone"}},
	}
}

// ServeFixture is `open-conduit extension-run claude-code certify-fixture`:
// in the runner's fresh directory it writes ./claude, prints the fixture
// and serves until stdin closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Join(dir, "claude-config"), 0o700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "claude"), []byte(fakeClaude), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(Fixture("{tmp}/claude"))
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(stdout, "%s\n", b); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, stdin)
	return nil
}
