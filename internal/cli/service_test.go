package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
)

// fakeServiceManager answers launchctl and systemctl like a machine whose
// unit is loaded and running once started.
type fakeServiceManager struct {
	loaded, running bool
	calls           []string
}

func (f *fakeServiceManager) run(name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	switch {
	case strings.Contains(call, "launchctl print"):
		if !f.loaded {
			return "Could not find service", errors.New("exit 113")
		}
		if f.running {
			return "\tstate = running\n\tpid = 4242\n", nil
		}
		return "\tstate = not running\n", nil
	case strings.Contains(call, "launchctl bootstrap"):
		f.loaded, f.running = true, true
	case strings.Contains(call, "launchctl bootout"):
		f.loaded, f.running = false, false
	case strings.Contains(call, "systemctl --user show"):
		if f.running {
			return "ActiveState=active\nMainPID=4242\n", nil
		}
		return "ActiveState=inactive\nMainPID=0\n", nil
	case strings.Contains(call, "systemctl --user start"):
		f.running = true
	case strings.Contains(call, "systemctl --user stop"):
		f.running = false
	}
	return "", nil
}

func TestServiceLifecycle(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			root := t.TempDir()
			env := map[string]string{"HOME": root, "PATH": "/home/me/go/bin:relative"}
			fake := &fakeServiceManager{}
			host := &ServiceHost{GOOS: goos, Executable: "/opt/oc/open-conduit", UID: 501, Run: fake.run,
				LookPath: func(name string) (string, error) {
					if name == "claude" || name == "cloudflared" {
						return "/Users/me/.local/bin/" + name, nil
					}
					return "", errors.New("not found")
				}}
			run := func(args ...string) (string, string, int) {
				var out, errOut bytes.Buffer
				e := Env{Getenv: func(k string) string { return env[k] }, Cwd: root, Service: host}
				code := RunOperator("test", "abc", args, e, &out, &errOut)
				return out.String(), errOut.String(), code
			}

			if out, _, code := run("service", "status"); code != 3 || !strings.Contains(out, "not installed") {
				t.Fatalf("status before install: %d %q", code, out)
			}
			// A tunnel stored in settings.webhooks needs its binary like a --tunnel flag.
			if err := os.WriteFile(filepath.Join(root, "stored.json"), []byte(`{"settings": {"webhooks": {"addr": "127.0.0.1:8787", "tunnel": "ngrok"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, errOut, code := run("service", "install", "--no-start", "--config", "stored.json"); code != 0 || !strings.Contains(out, "warning: ngrok is not on PATH") {
				t.Fatalf("install with a stored tunnel: %d %q %s", code, out, errOut)
			}
			out, errOut, code := run("service", "install", "--webhook-addr", "127.0.0.1:8787", "--tunnel", "cloudflared", "--config", "cfg.json")
			if code != 0 {
				t.Fatalf("install: %d %s %s", code, out, errOut)
			}
			if !strings.Contains(out, "warning: git is not on PATH") || strings.Contains(out, "claude is not") {
				t.Fatalf("missing tools not reported: %s", out)
			}
			unitPath := filepath.Join(root, "Library", "LaunchAgents", "com.open-conduit.daemon.plist")
			if goos == "linux" {
				unitPath = filepath.Join(root, ".config", "systemd", "user", "open-conduit.service")
			}
			unit, err := os.ReadFile(unitPath)
			if err != nil {
				t.Fatal(err)
			}
			stateDir := filepath.Join(root, ".local", "state", "open-conduit")
			log := filepath.Join(stateDir, "logs", "service.log")
			for _, want := range []string{"/opt/oc/open-conduit", stateDir, filepath.Join(root, "cfg.json"), "127.0.0.1:8787", "cloudflared",
				"/home/me/go/bin:/Users/me/.local/bin:/opt/homebrew/bin", log} {
				if !strings.Contains(string(unit), want) {
					t.Fatalf("unit lacks %q:\n%s", want, unit)
				}
			}
			if strings.Contains(string(unit), "relative") || strings.Count(string(unit), "cfg.json") != 1 {
				t.Fatalf("relative PATH entry kept or --config repeated:\n%s", unit)
			}
			if out, _, code = run("service", "status"); code != 0 || !strings.Contains(out, "running (pid 4242)") || !strings.Contains(out, log) {
				t.Fatalf("status after install: %d %q", code, out)
			}

			// doctor, with the daemon itself unreachable, puts the service after it.
			out, _, _ = run("doctor", "--json")
			var d client.Doctor
			if err := json.Unmarshal([]byte(out), &d); err != nil || len(d.Checks) < 2 || d.Checks[1].Name != "service" || d.Checks[1].Code != "running" {
				t.Fatalf("doctor service check: %s", out)
			}

			if out, _, code = run("service", "stop"); code != 0 || !strings.Contains(out, "stopped") {
				t.Fatalf("stop: %d %q", code, out)
			}
			if out, _, code = run("service", "status"); code != 3 || !strings.Contains(out, "stopped") {
				t.Fatalf("status after stop: %d %q", code, out)
			}
			o := &operator{env: Env{Getenv: func(k string) string { return env[k] }, Service: host}, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
			if c, ok := o.serviceCheck(); !ok || c.Status != "warn" || c.Code != "stopped" {
				t.Fatalf("doctor check when stopped: %+v", c)
			}
			if _, _, code = run("service", "start"); code != 0 || !fake.running {
				t.Fatalf("start: %d", code)
			}
			if out, _, code = run("service", "uninstall"); code != 0 || !strings.Contains(out, "removed") || fake.running {
				t.Fatalf("uninstall: %d %q", code, out)
			}
			if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unit left behind: %v", err)
			}
			if c, ok := o.serviceCheck(); !ok || c.Code != "not_installed" {
				t.Fatalf("doctor check after uninstall: %+v", c)
			}
			if goos == "linux" && !strings.Contains(strings.Join(fake.calls, "\n"), "systemctl --user enable open-conduit.service") {
				t.Fatalf("unit not enabled: %v", fake.calls)
			}
		})
	}
}

func TestServiceUnsupportedPlatform(t *testing.T) {
	var out, errOut bytes.Buffer
	e := Env{Getenv: func(string) string { return "" }, Service: &ServiceHost{GOOS: "windows"}}
	if code := RunOperator("test", "abc", []string{"service", "status"}, e, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "macOS") {
		t.Fatalf("windows: %d %q", code, errOut.String())
	}
}

func TestSystemdQuote(t *testing.T) {
	if got := systemdQuote(`/a b/100%$x"y`); got != `"/a b/100%%$$x\"y"` {
		t.Fatal(got)
	}
}
