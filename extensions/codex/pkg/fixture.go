package codexext

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
)

// Certification is the package's certification.json: a fixture command
// that writes a fake Codex CLI into the runner's directory and names it as
// the binding's binary, so certification never runs the real one.
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Command: []string{"codex-extension", "certify-fixture"}}
}

// barrierEnv names a file the fake's first turn of a thread waits for.
const barrierEnv = "CODEX_FAKE_BARRIER"

// Fixture is the certification fixture for a fake codex at binary: one
// turn for the executor case, and for the inject case a first turn that
// waits for the runner's finish file, so the injected Event becomes the
// segment's follow-up turn.
func Fixture(binary string) protocol.Fixture {
	run, _ := json.Marshal(map[string]any{"binary": binary})
	inject, _ := json.Marshal(map[string]any{"binary": binary, "env": map[string]string{barrierEnv: "{tmp}/inject-go"}})
	return protocol.Fixture{
		Executor:      &protocol.ExecutorCase{Executor: Name, Config: protocol.Payload{Data: run}},
		Inject:        &protocol.InjectCase{Executor: Name, Config: protocol.Payload{Data: inject}, FinishFile: "{tmp}/inject-go"},
		ExecutorModes: []protocol.ExecutorModeCase{{Executor: Name, Config: json.RawMessage(`{}`), Mode: "work"}, {Executor: Name, Config: json.RawMessage(`{"workspace_mode":"clone"}`), Mode: "clone"}},
	}
}

// ServeFixture is `codex-extension certify-fixture`: in the runner's fresh
// directory it writes ./codex, which runs this binary's fake-codex, prints
// the fixture and serves until stdin closes.
func ServeFixture(stdin io.Reader, stdout io.Writer) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	script := fmt.Sprintf("#!/bin/sh\nexec '%s' fake-codex \"$@\"\n", self)
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

// FakeCodex is `codex-extension fake-codex`: one `codex exec --json` turn
// of a thread called fake-thread. A fresh thread's turn waits for the
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
	if barrier := os.Getenv(barrierEnv); barrier != "" && !resume {
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
