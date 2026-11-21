package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestoreInPlaceCommands: backup restore --in-place restores into the
// daemon's own state directory and takes no --to; serve --restore restores
// in place, once, before serving, and doesn't serve when the restore fails
// or the restored directory's check does; service install refuses
// --restore, which would run on every start.
func TestRestoreInPlaceCommands(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	type call struct {
		bundle, stateDir string
		once             bool
	}
	var calls []call
	var artifacts, startConfig string
	var served int
	var result BackupInfo
	var failure error
	run := func(args ...string) (string, int) {
		var out, errOut bytes.Buffer
		e := Env{Getenv: func(k string) string {
			if k == "OPEN_CONDUIT_DATA_DIR" {
				return state
			}
			return ""
		}, Cwd: root, Serve: func(ServeRequest) int { served++; return 0 },
			State: &StateOps{RestoreInPlace: func(_ context.Context, bundle, stateDir string, o RestoreOptions) (BackupInfo, error) {
				calls = append(calls, call{bundle, stateDir, o.Once})
				artifacts, startConfig = o.ArtifactBackend, o.StartConfig
				return result, failure
			}},
			Service: &ServiceHost{GOOS: "linux", Executable: "/opt/oc/open-conduit"}}
		code := RunOperator("test", "abc", args, e, &out, &errOut)
		return errOut.String(), code
	}
	ok := BackupInfo{InPlace: &InPlaceReport{Check: &StateReport{OK: true}}}

	result = ok
	if msg, code := run("backup", "restore", "b", "--in-place"); code != 0 || len(calls) != 1 || calls[0] != (call{filepath.Join(root, "b"), state, false}) {
		t.Fatalf("--in-place: %d %q %+v", code, msg, calls)
	}
	if msg, code := run("backup", "restore", "b", "--in-place", "--to", "x"); code == 0 || !strings.Contains(msg, "not both") {
		t.Fatalf("--in-place --to: %d %q", code, msg)
	}
	result = BackupInfo{InPlace: &InPlaceReport{Check: &StateReport{OK: false}}}
	if _, code := run("backup", "restore", "b", "--in-place"); code != 1 {
		t.Fatalf("a failed check exits %d", code)
	}

	calls = nil
	result = ok
	if msg, code := run("serve", "--restore", "b", "--restore-artifacts", "files", "--restore-start-config", "new.json"); code != 0 || served != 1 || len(calls) != 1 || !calls[0].once || artifacts != "files" || startConfig != filepath.Join(root, "new.json") {
		t.Fatalf("serve --restore: %d %q served %d %+v", code, msg, served, calls)
	}
	failure = errors.New("bad bundle")
	if _, code := run("serve", "--restore", "b"); code != 1 || served != 1 {
		t.Fatalf("a failed restore served: %d, served %d", code, served)
	}
	failure, result = nil, BackupInfo{InPlace: &InPlaceReport{Check: &StateReport{OK: false}}}
	if _, code := run("serve", "--restore", "b"); code != 1 || served != 1 {
		t.Fatalf("a failed check served: %d, served %d", code, served)
	}
	// The negative control: serve without --restore restores nothing.
	calls = nil
	if _, code := run("serve"); code != 0 || served != 2 || len(calls) != 0 {
		t.Fatalf("plain serve: %d, served %d, %+v", code, served, calls)
	}

	if msg, code := run("serve", "--restore-artifacts", "files"); code == 0 || !strings.Contains(msg, "go with --restore") || served != 2 {
		t.Fatalf("--restore-artifacts alone: %d %q", code, msg)
	}
	if msg, code := run("serve", "--restore-start-config", "new.json"); code == 0 || !strings.Contains(msg, "goes with --restore") || served != 2 {
		t.Fatalf("--restore-start-config alone: %d %q", code, msg)
	}
	if msg, code := run("service", "install", "--restore", "b"); code == 0 || !strings.Contains(msg, "takes no --restore") {
		t.Fatalf("service install --restore: %d %q", code, msg)
	}
}
