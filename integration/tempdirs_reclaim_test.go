package integration_test

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// tempDirHolderEnv makes the test binary a run that holds a run root and a
// shared build, prints their paths and blocks until killed.
const tempDirHolderEnv = "OC_TEST_TEMPDIR_HOLDER"

func tempDirHolderMain(base string) int {
	root, lock, err := newRunRoot(base)
	if err != nil {
		return 1
	}
	defer lock.Close()
	shared, err := sharedBuildAt(base, "holder", "key", func() (string, error) { return "key", nil }, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "bin"), []byte("built"), 0o644)
	})
	if err != nil {
		return 1
	}
	os.Stdout.WriteString(root + "\n" + shared + "\n")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	return 0
}

// startTempDirHolder is a live helper run under base, with the paths it holds.
func startTempDirHolder(t *testing.T, base string) (*exec.Cmd, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), tempDirHolderEnv+"="+base)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	out := bufio.NewReader(stdout)
	root, err := out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	shared, err := out.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return cmd, strings.TrimSpace(root), strings.TrimSpace(shared)
}

func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A run killed with SIGKILL runs no cleanup; the next run reclaims what it
// held. Negative control: while it is alive, the same reclaim keeps both.
func TestReclaimRemovesAKilledRunsDirsAndKeepsALiveRuns(t *testing.T) {
	base := t.TempDir()
	cmd, root, shared := startTempDirHolder(t, base)

	reclaimDirs(base)
	if !dirExists(root) || !dirExists(shared) {
		t.Fatalf("reclaim removed a live run's directories (root %v, shared %v)", dirExists(root), dirExists(shared))
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	reclaimDirs(base)
	if dirExists(root) || dirExists(shared) {
		t.Fatalf("reclaim kept a killed run's directories (root %v, shared %v)", dirExists(root), dirExists(shared))
	}
}

// A root from a checkout before the locks holds none, so a free lock on it
// says nothing: reclaim must leave it alone.
func TestReclaimLeavesUnlockedLegacyRoots(t *testing.T) {
	base := t.TempDir()
	legacy := filepath.Join(base, "oc-test-run-123")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	reclaimDirs(base)
	if !dirExists(legacy) {
		t.Fatal("reclaim removed a root that predates the locks")
	}
}

// Parallel runs of the same code build once and share the result.
func TestSharedBuildBuildsOnceForConcurrentRuns(t *testing.T) {
	base := t.TempDir()
	var builds atomic.Int32
	var wg sync.WaitGroup
	paths := make([]string, 8)
	errs := make([]error, len(paths))
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i], errs[i] = sharedBuildAt(base, "once", "key", func() (string, error) { return "key", nil }, func(dir string) error {
				builds.Add(1)
				return os.WriteFile(filepath.Join(dir, "bin"), []byte("built"), 0o644)
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		if paths[i] != paths[0] {
			t.Fatalf("runs got different builds: %s and %s", paths[i], paths[0])
		}
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("built %d times, want 1", n)
	}
}

// What a builder killed part way left is rebuilt, never reused.
func TestSharedBuildRebuildsAnUnfinishedOne(t *testing.T) {
	base := t.TempDir()
	partial := filepath.Join(base, sharedBuildPrefix+"partial-key")
	if err := os.Mkdir(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "bin"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := sharedBuildAt(base, "partial", "key", func() (string, error) { return "key", nil }, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "bin"), []byte("built"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "bin"))
	if err != nil || string(got) != "built" {
		t.Fatalf("bin = %q, %v", got, err)
	}
}

// A failed build publishes nothing for the next run to pick up.
func TestSharedBuildDoesNotPublishAFailure(t *testing.T) {
	base := t.TempDir()
	boom := errors.New("build failed")
	rekey := func() (string, error) { return "key", nil }
	if _, err := sharedBuildAt(base, "failed", "key", rekey, func(string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if buildReady(filepath.Join(base, sharedBuildPrefix+"failed-key")) {
		t.Fatal("a failed build was published")
	}
}

// Something another user could have planted at a shared build's predictable
// path is neither run nor removed. The first case is the negative control: the
// same directory, private to this user, is what a run reuses.
func TestSharedBuildRefusesADirectoryOthersCanWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   os.FileMode
		reused bool
	}{
		{"private", 0o700, true},
		{"group_writable", 0o770, false},
		{"world_writable", 0o777, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			planted := filepath.Join(base, sharedBuildPrefix+"planted-key")
			if err := os.Mkdir(planted, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"bin", sharedReadyFile} {
				if err := os.WriteFile(filepath.Join(planted, f), []byte("planted"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(planted, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(planted, 0o700) })
			built := false
			dir, err := sharedBuildAt(base, "planted", "key", func() (string, error) { return "key", nil }, func(dir string) error {
				built = true
				return os.WriteFile(filepath.Join(dir, "bin"), []byte("built"), 0o755)
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := dir == planted; got != tc.reused {
				t.Fatalf("used the directory at the shared path: %v, want %v", got, tc.reused)
			}
			if built == tc.reused {
				t.Fatalf("built: %v, with reused %v", built, tc.reused)
			}
			want := "built"
			if tc.reused {
				want = "planted"
			}
			if got, err := os.ReadFile(filepath.Join(dir, "bin")); err != nil || string(got) != want {
				t.Fatalf("bin = %q, %v, want %q", got, err, want)
			}
			if !tc.reused {
				reclaimDirs(base)
				if !dirExists(filepath.Join(planted, "bin")) {
					t.Fatal("reclaim removed a directory it refused to trust")
				}
			}
		})
	}
}

// A directory another user owns is not private, even with safe permissions.
func TestDirIsPrivateChecksTheOwner(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !dirIsPrivate(info, os.Getuid()) {
		t.Fatal("this user's own temp directory is not private")
	}
	if dirIsPrivate(info, os.Getuid()+1) {
		t.Fatal("a directory owned by another uid counted as private")
	}
}

// A symlink at the shared path is refused rather than followed.
func TestSharedBuildRefusesASymlink(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(base, sharedBuildPrefix+"linked-key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dir, err := sharedBuildAt(base, "linked", "key", func() (string, error) { return "key", nil }, func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "bin"), []byte("built"), 0o755)
	})
	if err != nil {
		t.Fatal(err)
	}
	if dir == link || dirExists(filepath.Join(target, "bin")) {
		t.Fatalf("built through the symlink: %s", dir)
	}
}
