package integration_test

import (
	"os/exec"
	"strings"
	"testing"
)

// scripts/check-temp-dir.sh, which make test-unit and test-integration run
// under, fails a command that leaves anything in the temp directory and
// passes one that doesn't (#445).
func TestCheckTempDirScript(t *testing.T) {
	run := func(script string) (string, error) {
		cmd := exec.Command("scripts/check-temp-dir.sh", "sh", "-c", script)
		cmd.Dir = ".."
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run(`d=$(mktemp -d); touch "$d/x"; rm -rf "$d"`); err != nil {
		t.Fatalf("a command that cleans up failed the check: %v\n%s", err, out)
	}
	// Negative control: a leaked directory fails the check and is named.
	out, err := run(`mkdir "$TMPDIR/oc-leaked-bin-1"`)
	if err == nil || !strings.Contains(out, "oc-leaked-bin-1") {
		t.Fatalf("a leaked directory passed the check (%v):\n%s", err, out)
	}
	// A failing command keeps its own exit status.
	if _, err = run(`exit 3`); err == nil {
		t.Fatal("a failing command passed the check")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Fatalf("a command exiting 3 gave %v", err)
	}
}
