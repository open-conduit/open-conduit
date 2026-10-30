package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestSweepFindsSandboxedChildren: a process running in an srt sandbox
// (B09), tied to a workspace only by its working directory, still counts as
// using the workspace, and the sweep's kill still stops it. It needs srt;
// the sandbox CI jobs (OPEN_CONDUIT_SANDBOX_TESTS=1) run it.
func TestSweepFindsSandboxedChildren(t *testing.T) {
	bin := os.Getenv("OPEN_CONDUIT_SRT")
	if bin == "" {
		bin, _ = exec.LookPath("srt")
	}
	if bin == "" {
		if os.Getenv("OPEN_CONDUIT_SANDBOX_TESTS") == "1" {
			t.Fatal("OPEN_CONDUIT_SANDBOX_TESTS=1, but srt is not installed")
		}
		t.Skip("srt is not installed; the sandbox CI jobs run this test")
	}
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	settings := filepath.Join(t.TempDir(), "srt.json")
	doc := `{"network":{"allowedDomains":[],"deniedDomains":[]},"filesystem":{"denyRead":[],"allowRead":[],"allowWrite":[{"path":"` + ws + `","literal":true}],"denyWrite":[]}}`
	if err := os.WriteFile(settings, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--settings", settings, "--", "sh", "-c", "sleep 300 & wait")
	cmd.Dir = ws
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for !inUse(ws) {
		if time.Now().After(deadline) {
			t.Fatalf("a sandboxed process in the workspace was not found (%s)", runtime.GOOS)
		}
		time.Sleep(100 * time.Millisecond)
	}
	killWithin(ws)
	deadline = time.Now().Add(10 * time.Second)
	for inUse(ws) {
		if time.Now().After(deadline) {
			t.Fatal("the sweep's kill left the sandboxed process running")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
