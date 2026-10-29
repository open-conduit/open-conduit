package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An upgrade replaces the install directory under a running daemon: Homebrew
// renames the old version's directory and deletes it. The daemon notices its
// toolbelt is gone and exits non-zero, so the service manager starts the new
// version instead of the old one handing executions a missing `os`.
func TestDaemonStopsWhenItsInstallIsRemoved(t *testing.T) {
	d := newDrill(t)
	install := filepath.Join(d.root, "install")
	if err := os.Mkdir(install, 0o755); err != nil {
		t.Fatal(err)
	}
	built := filepath.Dir(daemonBinary(t))
	for _, name := range []string{"open-conduit", "os"} {
		b, err := os.ReadFile(filepath.Join(built, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(install, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.bin = filepath.Join(install, "open-conduit")
	d.start("")
	if err := os.Remove(filepath.Join(install, "os")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.exited:
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon kept running after its toolbelt was removed")
	}
	if code := d.cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code %d, want 1 so a service manager restarts it", code)
	}
	log, _ := os.ReadFile(d.log.Name())
	if !strings.Contains(string(log), "the install this daemon runs from is gone") || !strings.Contains(string(log), "restart to run the new version") {
		t.Fatalf("the log does not say why it stopped:\n%s", log)
	}
}
