package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtensionNewTS starts a TypeScript package with `extension new --lang
// ts` against this checkout, under a "type": "module" package.json, and runs
// its own `make check`. It then runs the built package the ways a daemon
// can: with no Node on PATH, and as a portable build on a machine whose only
// Node is the one on PATH. Nothing is fetched: it uses the weather example's
// node_modules, which holds this checkout's SDK and the tool versions
// TestTSScaffoldMatchesSDK holds the scaffold to. `make ts-deps` installs
// them, so it runs only when OPEN_CONDUIT_TS_SCAFFOLD_TESTS says that has
// happened (`make ts-scaffold-check`).
func TestExtensionNewTS(t *testing.T) {
	if os.Getenv("OPEN_CONDUIT_TS_SCAFFOLD_TESTS") == "" {
		t.Skip("run with make ts-scaffold-check")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	nodePath, err = filepath.EvalSymlinks(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "package.json"), []byte(`{"type":"module"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "acme")
	env := offlineEnv()
	run := func(cwd string, env []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = cwd, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	run(root, env, daemonBinary(t), "extension", "new", "acme", "--lang", "ts", "--dir", dir, "--replace", root)
	if err := os.Symlink(filepath.Join(root, "protocol", "examples", "weather", "node_modules"), filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	run(dir, env, "make", "check")
	run(dir, env, "make", "lint", "certify", "OPEN_CONDUIT="+daemonBinary(t))

	// The recorded Node runs it with nothing on PATH.
	bin := filepath.Join(dir, "dist", "acme", "acme-extension")
	if out := run(dir, []string{"PATH=" + t.TempDir()}, bin, "package-manifest"); !strings.Contains(out, `"name": "acme"`) {
		t.Fatalf("package-manifest with no node on PATH: %s", out)
	}

	// A portable build names nothing from this machine, runs with the Node
	// on PATH, and says what is wrong when there is none.
	run(dir, env, "make", "package", "PORTABLE=1", "PACKAGE_DIR=dist/portable")
	bin = filepath.Join(dir, "dist", "portable", "acme-extension")
	launcher, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(launcher), nodePath) {
		t.Fatalf("the portable launcher names this machine's node:\n%s", launcher)
	}
	onlyNode := t.TempDir()
	if err := os.Symlink(nodePath, filepath.Join(onlyNode, "node")); err != nil {
		t.Fatal(err)
	}
	if out := run(dir, []string{"PATH=" + onlyNode}, bin, "package-manifest"); !strings.Contains(out, `"name": "acme"`) {
		t.Fatalf("package-manifest with node on PATH: %s", out)
	}
	cmd := exec.Command(bin, "package-manifest")
	cmd.Env = []string{"PATH=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "no node on PATH") {
		t.Fatalf("a portable build with no node: %v\n%s", err, out)
	}
}
