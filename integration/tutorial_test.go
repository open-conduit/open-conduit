package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The "Build your first extension" pages, in Go and in TypeScript.
const (
	tutorial   = "../site/src/content/docs/guides/first-extension.md"
	tutorialTS = "../site/src/content/docs/guides/first-extension-typescript.md"
)

// titledFile matches the opening fence of a code block the tutorial asks the
// reader to save, such as ```go title="main.go".
var titledFile = regexp.MustCompile("^```[a-z]+ title=\"([^\"]+)\"$")

// tutorialFiles returns a tutorial's titled code blocks by file name, and
// every open-conduit command line in its sh blocks.
func tutorialFiles(t *testing.T, page string) (map[string]string, []string) {
	t.Helper()
	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	var commands []string
	var name string
	var cur *strings.Builder
	sh := false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case cur == nil && !sh && titledFile.MatchString(line):
			name, cur = titledFile.FindStringSubmatch(line)[1], &strings.Builder{}
		case cur == nil && !sh && line == "```sh":
			sh = true
		case (cur != nil || sh) && line == "```":
			if cur != nil {
				files[name] = cur.String()
			}
			cur, sh = nil, false
		case cur != nil:
			cur.WriteString(line + "\n")
		case sh && strings.HasPrefix(line, "open-conduit "):
			commands = append(commands, line)
		}
	}
	if cur != nil || sh {
		t.Fatalf("%s: unterminated code block", page)
	}
	return files, commands
}

// shellFields splits a command line as sh would, for the quoting the
// tutorial uses: single and double quotes, no escapes or substitutions.
func shellFields(t *testing.T, line string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	in, quote := false, rune(0)
	for _, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		t.Fatalf("unterminated quote in %q", line)
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

// TestTutorialFirstExtension follows "Build your first extension" as a
// reader would, so the tutorial can't rot: it starts the weather package
// with `extension new` (built against this checkout), saves every file the
// page shows over the scaffold's, and runs the repository's make check
// (its sdktest and extensiontest tests, the build and `extension check`).
// Then it installs the package beside a GitHub scope's config, runs the
// page's `config set` commands on it and checks the result validates.
func TestTutorialFirstExtension(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	files, commands := tutorialFiles(t, tutorial)
	for _, want := range []string{"main.go", "main_test.go", "e2e_test.go", "playbooks/weather.md", "fakeweather/main.go"} {
		if files[want] == "" {
			t.Fatalf("first-extension.md has no %s block", want)
		}
	}
	env := offlineEnv()
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
	bin := daemonBinary(t)
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "weather")
	run(root, bin, "extension", "new", "weather", "--dir", dir, "--replace", root)
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := run(dir, "make", "check", "TESTFLAGS=-count=1")
	if !strings.Contains(out, "ok  \texample.com/weather") || !strings.Contains(out, "no problems found") {
		t.Fatalf("make check:\n%s", out)
	}

	tutorialInstall(t, run, root, bin, tmp, filepath.Join(dir, "dist", "weather"), commands)
}

// tutorialInstall follows a tutorial's install step: the scope `init github
// --repo acme/web` writes, with the package installed beside it and the
// page's config set commands run, and checks the result validates.
func tutorialInstall(t *testing.T, run func(cwd, name string, args ...string) string, root, bin, tmp, pkg string, commands []string) {
	t.Helper()
	cfgDir := filepath.Join(tmp, "config")
	cfg := filepath.Join(cfgDir, "config.json")
	state := filepath.Join(tmp, "state")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	run(root, bin, "--data-dir", state, "init", "github", "--repo", "acme/web", "--checkout", root, "--config", cfg)
	run(root, bin, "--data-dir", state, "extension", "install", pkg, "--config", cfg)
	sets := 0
	for _, line := range commands {
		args := shellFields(t, line)[1:]
		if len(args) < 2 || args[0] != "config" || args[1] != "set" {
			continue
		}
		sets++
		run(root, bin, append(append([]string{"--data-dir", state}, args...), "--file", cfg)...)
	}
	if sets == 0 {
		t.Fatal("the tutorial shows no config set commands")
	}
	if out := run(root, bin, "--data-dir", state, "config", "check", "--file", cfg); !strings.Contains(out, "valid") {
		t.Fatalf("config check: %s", out)
	}
}

// TestTutorialFirstExtensionTS follows the TypeScript tutorial as a reader
// would: it starts the package with `extension new --lang ts`, saves every
// file the page shows over the scaffold's, runs make check, lint and
// certify, then installs it and runs the page's config set commands. It
// uses the weather example's node_modules, so like TestExtensionNewTS it
// runs under `make ts-scaffold-check`.
func TestTutorialFirstExtensionTS(t *testing.T) {
	if os.Getenv("OPEN_CONDUIT_TS_SCAFFOLD_TESTS") == "" {
		t.Skip("run with make ts-scaffold-check")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	files, commands := tutorialFiles(t, tutorialTS)
	for _, want := range []string{"src/extension.ts", "src/extension.test.ts", "playbooks/weather.md", "fakeweather.mjs"} {
		if files[want] == "" {
			t.Fatalf("first-extension-typescript.md has no %s block", want)
		}
	}
	env := offlineEnv()
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
	bin := daemonBinary(t)
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "weather")
	run(root, bin, "extension", "new", "weather", "--lang", "ts", "--dir", dir, "--replace", root)
	if err := os.Symlink(filepath.Join(root, "protocol", "examples", "weather", "node_modules"), filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(dir, "make", "check")
	run(dir, "make", "lint", "certify", "OPEN_CONDUIT="+bin)
	tutorialInstall(t, run, root, bin, tmp, filepath.Join(dir, "dist", "weather"), commands)
}
