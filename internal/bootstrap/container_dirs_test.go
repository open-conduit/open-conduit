package bootstrap

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestAdoptLegacyContainerDirs: the ssh provider's pinned host keys move
// once from <state>/remote to its package's <state>/containers/ssh, a file
// already there stays, and a second start moves nothing.
func TestAdoptLegacyContainerDirs(t *testing.T) {
	state := t.TempDir()
	old := filepath.Join(state, "remote")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"known_hosts-a": "old a", "known_hosts-b": "old b"} {
		if err := os.WriteFile(filepath.Join(old, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := containerDir(state, "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "known_hosts-b"), []byte("new b"), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err := adoptLegacyContainerDirs(state)
	if err != nil || moved != 1 {
		t.Fatalf("moved %d, %v; want 1", moved, err)
	}
	for name, want := range map[string]string{"known_hosts-a": "old a", "known_hosts-b": "new b"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Fatalf("%s: %q %v, want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("the old directory is still there: %v", err)
	}
	if moved, err := adoptLegacyContainerDirs(state); err != nil || moved != 0 {
		t.Fatalf("a second start moved %d, %v", moved, err)
	}
}

// TestContainerSwitches: a container provider package gets the host's
// container switches that are set, and nothing else of the daemon's
// OPEN_CONDUIT_* environment.
func TestContainerSwitches(t *testing.T) {
	got := containerSwitches([]string{"OPEN_CONDUIT_GATEWAY_IMAGE=sha256:x", "OPEN_CONDUIT_CONFIG=/c", "OPEN_CONDUIT_PASS_FILE=/p", "PATH=/bin"})
	if want := []string{"OPEN_CONDUIT_GATEWAY_IMAGE=sha256:x"}; !slices.Equal(got, want) {
		t.Fatalf("switches %v, want %v", got, want)
	}
}
