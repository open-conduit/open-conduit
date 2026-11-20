//go:build unix

package claudecode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runWrapperStoppedAtPID runs wrapper as a task's wrapper does, with an mv
// that signals the group with SIGTERM the moment the pid is published: the
// earliest a stop can reach the task (taskLeader reads the pid first). It
// reports whether the wrapper recorded an exit.
func runWrapperStoppedAtPID(t *testing.T, wrapper string) bool {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	mv, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\n" + mv + " \"$@\" || exit 1\ncase \"$*\" in *pid.tmp*) kill -TERM 0 ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "mv"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", wrapper, taskMarker+"test", dir, "/bin/sh", "-c", "exit 0")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	detach(cmd)
	_ = cmd.Run()
	if _, err := os.Stat(filepath.Join(dir, "pid")); err != nil {
		t.Fatalf("the wrapper published no pid: %v", err)
	}
	return exists(filepath.Join(dir, "exit"))
}

// A stop that reaches the task as soon as its pid is published still ends
// with an exit recorded, so the sweep announces it stopped, never lost
// (#794).
func TestTaskWrapperTrapsBeforePublishingPID(t *testing.T) {
	if !runWrapperStoppedAtPID(t, taskWrapper) {
		t.Fatal("a stop at the published pid ended the wrapper before it recorded an exit")
	}
}

// The negative control: a wrapper that publishes its pid before trapping
// is ended by that stop and records nothing.
func TestTaskWrapperPIDBeforeTrapIsNegativeControl(t *testing.T) {
	const trap = "trap ':' HUP INT TERM\n"
	lines := strings.SplitAfter(taskWrapper, "\n")
	if len(lines) < 3 || lines[1] != trap || !strings.Contains(lines[2], "/pid") {
		t.Fatalf("the wrapper's opening lines moved: %q", lines[:min(3, len(lines))])
	}
	lines[1], lines[2] = lines[2], lines[1]
	if runWrapperStoppedAtPID(t, strings.Join(lines, "")) {
		t.Fatal("the pid-first wrapper recorded an exit; the control doesn't reach the window")
	}
}
