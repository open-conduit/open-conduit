package integration_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtensionNew starts a package repository with `extension new`, built
// against this checkout (--replace), and runs the repository's own `make
// check` in it: formatting, vet, its sdktest and extensiontest tests, the
// build, packaging and `extension check`. Nothing is fetched: the module
// cache this checkout builds from already holds every dependency.
func TestExtensionNew(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	env := offlineEnv()
	dir := filepath.Join(t.TempDir(), "acme")
	run := func(cwd, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = cwd, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	out := run(root, daemonBinary(t), "extension", "new", "acme", "--dir", dir, "--module", "example.com/acme", "--replace", root)
	if !strings.Contains(out, "Created the acme package in "+dir) {
		t.Fatalf("extension new: %s", out)
	}
	for _, f := range []string{"go.mod", "go.sum", "main.go", "main_test.go", "e2e_test.go", "Makefile", ".github/workflows/ci.yml", ".github/workflows/release.yml", ".gitignore", "playbooks/acme.md", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	gomod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
	for _, want := range []string{"module example.com/acme", "github.com/open-conduit/open-conduit/cmd/open-conduit", "github.com/open-conduit/open-conduit/cmd/os", "replace github.com/open-conduit/open-conduit => " + root} {
		if !strings.Contains(string(gomod), want) {
			t.Errorf("go.mod has no %q:\n%s", want, gomod)
		}
	}

	// The repository's check, without -race: the integration run already
	// has it, and the daemon the package's tests build is slower with it.
	out = run(dir, "make", "check", "TESTFLAGS=-count=1")
	if !strings.Contains(out, "ok  \texample.com/acme") || !strings.Contains(out, filepath.Join("acme", "dist", "acme")+"): no problems found") {
		t.Fatalf("make check:\n%s", out)
	}

	// The release archive installs, and under settings.extension_trust only
	// once its digest is pinned: unsigned, no publisher admits it.
	out = run(dir, "make", "archive", "VERSION=1.2.3")
	archive := filepath.Join(dir, strings.TrimSpace(run(dir, "make", "-s", "archive-name", "VERSION=1.2.3")))
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("make archive: %v\n%s", err, out)
	}
	sum := sha256.Sum256(data)
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeTrust := func(trust string) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(`{"version":1,"settings":{"extension_trust":`+trust+`},"connections":[],"scopes":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeTrust(`{"publishers":[{"subject_regexp":"https://github\\.com/acme/acme/.+","issuer":"https://token.actions.githubusercontent.com"}]}`)
	cmd := exec.Command(daemonBinary(t), "extension", "install", archive, "--config", configPath)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "untrusted_package") || !strings.Contains(string(out), "no signature bundle") {
		t.Fatalf("an unsigned archive: %v\n%s", err, out)
	}
	writeTrust(`{"pinned":{"acme":["sha256:` + hex.EncodeToString(sum[:]) + `"]}}`)
	if out := run(dir, daemonBinary(t), "extension", "install", archive, "--config", configPath); !strings.Contains(out, "Installed acme 1.2.3") || !strings.Contains(out, "publisher: pinned digest") {
		t.Fatalf("a pinned archive: %s", out)
	}
	if out := run(dir, daemonBinary(t), "extension", "list", "--config", configPath); !strings.Contains(out, "publisher: pinned digest") || strings.Contains(out, "refused") {
		t.Fatalf("extension list: %s", out)
	}

	// A second run into the same directory is refused rather than
	// overwriting it.
	cmd = exec.Command(daemonBinary(t), "extension", "new", "acme", "--dir", dir, "--replace", root)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "already exists and is not empty") {
		t.Fatalf("a non-empty directory: %v\n%s", err, out)
	}
}

// offlineEnv is the environment a package repository's commands run in:
// offline, building from the module cache this checkout builds from, and
// free of the make that may be running the test.
func offlineEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "MAKEFLAGS" && k != "MFLAGS" && k != "MAKELEVEL" {
			env = append(env, kv)
		}
	}
	return append(env, "GOFLAGS=-mod=mod", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off")
}
