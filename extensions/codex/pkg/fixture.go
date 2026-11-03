package codexext

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Certification is the package's certification.json: a fixture command
// that writes a fake Codex CLI into the runner's directory and names it as
// the binding's binary, so certification never runs the real one.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: append(protocol.HostRunCommand(Name), "certify-fixture")}
}

// barrierEnv names a file every turn of the fake waits for.
const barrierEnv = "CODEX_FAKE_BARRIER"

// Fixture is the certification fixture for a fake codex at binary: one
// turn for the executor case, and for the inject case turns that wait for
// the runner's finish file, so the segment's first turn is still running
// when the runner injects and the injected Event becomes its follow-up
// turn. The first turn may resume a thread: the cases share a Work, and
// the executor case has already started one for it.
func Fixture(binary string) protocol.Fixture {
	run, _ := json.Marshal(map[string]any{"binary": binary})
	inject, _ := json.Marshal(map[string]any{"binary": binary, "env": map[string]string{barrierEnv: "{tmp}/inject-go"}})
	return protocol.Fixture{
		Executor:      &protocol.ExecutorCase{Executor: Name, Config: protocol.Payload{Data: run}},
		Inject:        &protocol.InjectCase{Executor: Name, Config: protocol.Payload{Data: inject}, FinishFile: "{tmp}/inject-go"},
		ExecutorModes: []protocol.ExecutorModeCase{{Executor: Name, Config: json.RawMessage(`{}`), Mode: "work"}, {Executor: Name, Config: json.RawMessage(`{"workspace_mode":"clone"}`), Mode: "clone"}},
	}
}

// ServeFixture is `open-conduit extension-run codex certify-fixture`: in the runner's fresh
// directory it writes ./codex, which runs this binary's fake-codex, prints
// the fixture and serves until stdin closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	self, err := sdk.Self()
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	quoted := make([]string, len(self))
	for i, a := range self {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s fake-codex \"$@\"\n", strings.Join(quoted, " "))
	if err = os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(Fixture("{tmp}/codex"))
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(stdout, "%s\n", b); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, stdin)
	return nil
}

// FakeCodex is `open-conduit extension-run codex fake-codex`: one `codex exec --json` turn
// of a thread called fake-thread. A turn, fresh or resumed, waits for the
// barrier file when CODEX_FAKE_BARRIER names one. Every turn ends with the
// status review.
func FakeCodex(args []string, stdin io.Reader, stdout io.Writer) int {
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
	_, _ = io.Copy(io.Discard, stdin)
	out := json.NewEncoder(stdout)
	if !resume {
		_ = out.Encode(map[string]any{"type": "thread.started", "thread_id": "fake-thread"})
	}
	_ = out.Encode(map[string]any{"type": "turn.started"})
	if barrier := os.Getenv(barrierEnv); barrier != "" {
		for {
			if _, err := os.Stat(barrier); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	_ = out.Encode(map[string]any{"type": "turn.completed", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
	if last != "" {
		status := `{"state":"review","summary":"certified","handoff":"","report":""}`
		if err := os.WriteFile(last, []byte(status), 0o600); err != nil {
			return 1
		}
	}
	return 0
}
