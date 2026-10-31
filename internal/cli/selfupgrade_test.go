package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/internal/cli/upgrade"
)

// releaseServer serves v1.9.0 as GitHub would, for linux/amd64.
func releaseServer(t *testing.T) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"open-conduit", "os"} {
		body := "new " + name
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	data := buf.Bytes()
	sum := sha256.Sum256(data)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/"+upgrade.Repo+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name": "v1.9.0", "html_url": "https://example.test/v1.9.0"}`))
	})
	base := "/" + upgrade.Repo + "/releases/download/v1.9.0/"
	mux.HandleFunc("GET "+base+"checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  open-conduit_1.9.0_linux_amd64.tar.gz\n"))
	})
	mux.HandleFunc("GET "+base+"open-conduit_1.9.0_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUpgrade(t *testing.T) {
	srv := releaseServer(t)
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "lib")
			_ = os.MkdirAll(bin, 0o755)
			for _, name := range []string{"open-conduit", "os"} {
				_ = os.WriteFile(filepath.Join(bin, name), []byte("old "+name), 0o755)
			}
			u := &upgrade.Updater{Executable: filepath.Join(bin, "open-conduit"), API: srv.URL, Download: srv.URL, GOOS: "linux", GOARCH: "amd64", HTTP: srv.Client()}
			fake := &fakeServiceManager{}
			host := &ServiceHost{GOOS: goos, Executable: u.Executable, UID: 501, Run: fake.run, LookPath: func(string) (string, error) { return "/usr/bin/x", nil }}
			env := map[string]string{"HOME": root}
			run := func(version string, args ...string) (string, string, int) {
				var out, errOut bytes.Buffer
				e := Env{Getenv: func(k string) string { return env[k] }, Cwd: root, Service: host, Updater: u}
				code := RunOperator(version, "abc", args, e, &out, &errOut)
				return out.String(), errOut.String(), code
			}

			if out, _, code := run("1.8.0", "upgrade", "--check"); code != 0 || !strings.Contains(out, "1.9.0 is out") || !strings.Contains(out, "run open-conduit upgrade") {
				t.Fatalf("check: %d %q", code, out)
			}
			out, _, code := run("1.9.0", "upgrade", "--check", "--json")
			var res upgradeResult
			if code != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Newer || res.Latest != "1.9.0" {
				t.Fatalf("check json: %d %q", code, out)
			}
			if out, _, code := run("1.9.0", "upgrade"); code != 0 || !strings.Contains(out, "up to date") {
				t.Fatalf("up to date: %d %q", code, out)
			}
			if b, _ := os.ReadFile(u.Executable); string(b) != "old open-conduit" {
				t.Fatal("installed while up to date")
			}

			// No service: install, and say serve needs a restart.
			if out, errOut, code := run("1.8.0", "upgrade"); code != 0 || !strings.Contains(out, "installed open-conduit 1.9.0 in "+bin) || !strings.Contains(out, "no background service is running") {
				t.Fatalf("upgrade without a service: %d %q %s", code, out, errOut)
			}
			for _, name := range []string{"open-conduit", "os"} {
				if b, _ := os.ReadFile(filepath.Join(bin, name)); string(b) != "new "+name {
					t.Fatalf("%s: %q", name, b)
				}
			}

			// A running service restarts on the new binary.
			if _, errOut, code := run("1.8.0", "service", "install", "--config", "cfg.json"); code != 0 {
				t.Fatalf("service install: %s", errOut)
			}
			fake.calls = nil
			out, errOut, code := run("1.8.0", "upgrade")
			if code != 0 || !strings.Contains(out, "restarted the background service on 1.9.0") {
				t.Fatalf("upgrade with a service: %d %q %s", code, out, errOut)
			}
			want := "launchctl kickstart -k gui/501/" + launchdLabel
			if goos == "linux" {
				want = "systemctl --user --no-block restart " + systemdUnit
			}
			if !contains(fake.calls, want) {
				t.Fatalf("no restart: %v", fake.calls)
			}
			fake.calls = nil
			if _, _, code := run("1.8.0", "upgrade", "--no-restart"); code != 0 || contains(fake.calls, want) {
				t.Fatalf("--no-restart restarted: %v", fake.calls)
			}
			if c := NewServiceControl(host, func(k string) string { return env[k] }); !c.Running() {
				t.Fatal("service control doesn't see the running service")
			}
		})
	}
}

func TestUpgradeLeavesHomebrewToBrew(t *testing.T) {
	srv := releaseServer(t)
	u := &upgrade.Updater{Executable: "/opt/homebrew/Caskroom/open-conduit/1.8.0/open-conduit", API: srv.URL, Download: srv.URL, HTTP: srv.Client()}
	var out, errOut bytes.Buffer
	e := Env{Getenv: func(string) string { return "" }, Updater: u}
	if code := RunOperator("1.8.0", "abc", []string{"upgrade"}, e, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), upgrade.BrewCommand) {
		t.Fatalf("homebrew: %d %q", code, errOut.String())
	}
	out.Reset()
	if code := RunOperator("1.8.0", "abc", []string{"upgrade", "--check"}, e, &out, &errOut); code != 0 || !strings.Contains(out.String(), "run "+upgrade.BrewCommand) {
		t.Fatalf("homebrew check: %d %q", code, out.String())
	}
}

func contains(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
