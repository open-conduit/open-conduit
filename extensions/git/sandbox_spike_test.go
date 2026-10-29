//go:build sandboxspike

// SPIKE #161: does the stray-process sweep see a sandboxed child?
// SPIKE_SRT=/path/to/srt go test -tags sandboxspike -run Spike -v ./extensions/git/
package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSpikeSweepFindsSandboxedChildren(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	settings := filepath.Join(t.TempDir(), "srt.json")
	os.WriteFile(settings, []byte(`{"network":{"allowedDomains":[],"deniedDomains":[]},"filesystem":{"denyRead":[],"allowRead":[],"allowWrite":["`+ws+`"],"denyWrite":[]}}`), 0o600)
	// Not named in its argv: only the working directory ties it to ws.
	cmd := exec.Command(os.Getenv("SPIKE_SRT"), "-s", settings, "--", "sh", "-c", "sleep 300 & wait")
	cmd.Dir = ws
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	time.Sleep(3 * time.Second)
	t.Logf("inUse(workspace) with a sandboxed child running: %v (cwd matches: %v)", inUse(ws), cwdWithin(ws))
	killWithin(ws)
	time.Sleep(500 * time.Millisecond)
	t.Logf("after killWithin: inUse=%v, cwd matches=%v", inUse(ws), cwdWithin(ws))
}
