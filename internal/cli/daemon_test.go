package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

// health and doctor before any daemon has run, after one has stopped, and
// with the token missing while one runs (a distinct error).
func TestHealthAndDoctorWithoutDaemon(t *testing.T) {
	// A short root keeps the socket path under the unix limit.
	root, err := os.MkdirTemp("/tmp", "oc-cli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	env := map[string]string{"XDG_STATE_HOME": filepath.Join(root, "state"), "XDG_CONFIG_HOME": filepath.Join(root, "cfg"), "HOME": root}
	stateDir := filepath.Join(root, "state", "open-conduit")
	configPath := filepath.Join(root, "cfg", "open-conduit", "config.json")
	var offline []string
	run := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		e := Env{Getenv: func(k string) string { return env[k] }, Cwd: root, OfflineChecks: func(_ context.Context, path string, data []byte) []client.Check {
			offline = append(offline, path)
			return []client.Check{{Name: "config", Status: "ok", Code: "valid", Detail: path}, {Name: "claude_code", Status: "ok", Code: "installed"}, {Name: "skipped", Status: "info", Code: "daemon_not_running", Detail: "need a running daemon: storage, github"}}
		}}
		code := RunOperator("test", "abc", args, e, &out, &errOut)
		return out.String(), errOut.String(), code
	}

	// Never started: no state directory, no token.
	_, errOut, code := run("health")
	if code != 1 || !strings.Contains(errOut, "never been started") || !strings.Contains(errOut, "open-conduit serve") {
		t.Fatalf("health never started: %d %q", code, errOut)
	}
	// Without a config, doctor fails its config check but still reports.
	out, _, code := run("doctor")
	if code != 1 || !strings.Contains(out, "not_running") || !strings.Contains(out, "no config at") || !strings.Contains(out, "overall: fail") {
		t.Fatalf("doctor without config: %d %s", code, out)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"version": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code = run("doctor", "--json")
	var d client.Doctor
	if code != 0 || json.Unmarshal([]byte(out), &d) != nil {
		t.Fatalf("doctor offline: %d %s", code, out)
	}
	names := []string{}
	for _, c := range d.Checks {
		names = append(names, c.Name+"/"+c.Status)
	}
	if d.State != "warn" || strings.Join(names, " ") != "daemon/warn config/ok claude_code/ok skipped/info" || len(offline) != 1 || offline[0] != configPath {
		t.Fatalf("offline doctor: %s %v %v", d.State, names, offline)
	}
	if skipped := d.Checks[len(d.Checks)-1].Detail; !strings.Contains(skipped, "storage") || !strings.Contains(skipped, "github") {
		t.Fatalf("skipped checks not listed: %q", skipped)
	}

	// Stopped: a token from an earlier run, nothing listening.
	tokenPath := filepath.Join(stateDir, "private", "operator.token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"token_id": "t1", "token": "secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = run("health")
	if code != 1 || !strings.Contains(errOut, "no Open Conduit daemon is running;") || strings.Contains(errOut, "never") {
		t.Fatalf("health stopped: %d %q", code, errOut)
	}
	if out, _, code = run("doctor"); code != 0 || !strings.Contains(out, "skipped") {
		t.Fatalf("doctor stopped: %d %s", code, out)
	}

	// Running without the token file: still a token error, not "not running".
	if err := os.Remove(tokenPath); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(stateDir, "management.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, cmd := range []string{"health", "doctor"} {
		_, errOut, code = run(cmd)
		if code != 1 || !strings.Contains(errOut, "operator token file unavailable") || !strings.Contains(errOut, "a daemon is running") {
			t.Fatalf("%s with daemon, no token: %d %q", cmd, code, errOut)
		}
	}
}
