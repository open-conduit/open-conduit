//go:build linux

package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// A process reaped between the open and the read of its /proc files fails
// the read with ESRCH, not ENOENT. inspectProcess must report it absent:
// an adopted segment that exits while Wait observes it is finished, not an
// executor error.
func TestProcGoneAfterReapBetweenOpenAndRead(t *testing.T) {
	for _, name := range []string{"stat", "cmdline"} {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), name))
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(err)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_, err = f.Read(make([]byte, 4096))
		f.Close()
		if err == nil || !procGone(err) {
			t.Fatalf("%s read after reap: %v is not reported gone", name, err)
		}
		if _, ok, err := inspectProcess(cmd.Process.Pid); ok || err != nil {
			t.Fatalf("reaped process: alive %v, err %v", ok, err)
		}
	}
	if _, err := os.ReadFile("/proc/0/stat"); !procGone(err) {
		t.Fatalf("missing /proc entry: %v is not reported gone", err)
	}
}
