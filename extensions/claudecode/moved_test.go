//go:build unix

package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

const movedSession = "11111111-2222-4333-8444-555555555555"

// movedHome is a config directory holding a session filed under a workspace
// path that no longer exists, and the workspace's new path.
func movedHome(t *testing.T) (config, old, now string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config, old, now = filepath.Join(root, "config"), filepath.Join(root, "old"), filepath.Join(root, "new")
	if err := os.MkdirAll(now, 0o700); err != nil {
		t.Fatal(err)
	}
	src := TranscriptPath(config, old, movedSession)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(src), movedSession, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config, old, now
}

func TestRefileSessionMovesTranscriptAndItsDirectory(t *testing.T) {
	config, old, now := movedHome(t)
	moved, err := refileSession(config, []string{"", old}, now, movedSession)
	if err != nil || !moved {
		t.Fatalf("refile = %v, %v", moved, err)
	}
	dst := TranscriptPath(config, now, movedSession)
	if _, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), movedSession, "subagents")); err != nil {
		t.Errorf("the session's directory didn't move: %v", err)
	}
	if _, err := os.Lstat(TranscriptPath(config, old, movedSession)); !os.IsNotExist(err) {
		t.Errorf("the old transcript is still there: %v", err)
	}
	if again, err := refileSession(config, []string{old}, now, movedSession); err != nil || again {
		t.Errorf("a second refile = %v, %v", again, err)
	}
}

// The negative control: a path that still exists is another workspace.
func TestRefileSessionLeavesAPathThatStillExists(t *testing.T) {
	config, old, now := movedHome(t)
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if moved, err := refileSession(config, []string{old}, now, movedSession); err != nil || moved {
		t.Fatalf("refile = %v, %v", moved, err)
	}
	if _, err := os.Stat(TranscriptPath(config, old, movedSession)); err != nil {
		t.Errorf("the transcript was taken: %v", err)
	}
}

// The agent can write to the home, so a link planted at any step must not
// carry the host's move outside it.
func TestRefileSessionFollowsNoLink(t *testing.T) {
	t.Run("transcript", func(t *testing.T) {
		config, old, now := movedHome(t)
		outside := filepath.Join(filepath.Dir(config), "outside")
		if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		src := TranscriptPath(config, old, movedSession)
		if err := os.Remove(src); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, src); err != nil {
			t.Fatal(err)
		}
		if moved, err := refileSession(config, []string{old}, now, movedSession); err == nil || moved {
			t.Fatalf("refile = %v, %v", moved, err)
		}
		if _, err := os.Lstat(TranscriptPath(config, now, movedSession)); !os.IsNotExist(err) {
			t.Errorf("a link was refiled: %v", err)
		}
	})
	t.Run("destination", func(t *testing.T) {
		config, old, now := movedHome(t)
		outside := filepath.Join(filepath.Dir(config), "outside")
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Dir(TranscriptPath(config, now, movedSession))); err != nil {
			t.Fatal(err)
		}
		if moved, err := refileSession(config, []string{old}, now, movedSession); err == nil || moved {
			t.Fatalf("refile = %v, %v", moved, err)
		}
		if left, _ := os.ReadDir(outside); len(left) != 0 {
			t.Errorf("the move wrote through the link: %v", left)
		}
	})
	t.Run("source directory", func(t *testing.T) {
		config, old, now := movedHome(t)
		outside := filepath.Join(filepath.Dir(config), "outside")
		key := filepath.Dir(TranscriptPath(config, old, movedSession))
		if err := os.Rename(key, outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, key); err != nil {
			t.Fatal(err)
		}
		if moved, err := refileSession(config, []string{old}, now, movedSession); err == nil || moved {
			t.Fatalf("refile = %v, %v", moved, err)
		}
		if _, err := os.Stat(filepath.Join(outside, movedSession+".jsonl")); err != nil {
			t.Errorf("the file behind the link moved: %v", err)
		}
	})
}
