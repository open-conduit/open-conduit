package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyTree copies a fixture directory (files only, keeping relative paths).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// treeDigest hashes every file under dir, so a test can prove inputs were
// left untouched.
func treeDigest(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(path)
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel))
		h.Write(b)
		return nil
	})
	return string(h.Sum(nil))
}

// TestMigrateConfigCommand is 06-02's conversion acceptance on the synthetic
// v1 fixture: a preview writes nothing, the converted config validates
// offline against the daemon's registrations, the repository file and
// playbook additions are read but never changed, secrets map to references
// only (a literal never reaches any output), and writes refuse to replace a
// config or to land in Open Supervisor's directory.
func TestMigrateConfigCommand(t *testing.T) {
	home := t.TempDir()
	v1dir := filepath.Join(home, ".config", "open-supervisor")
	if err := os.MkdirAll(v1dir, 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("..", "internal", "migrate", "testdata", "v1-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	v1 := filepath.Join(v1dir, "config.yaml")
	if err := os.WriteFile(v1, src, 0o600); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join("..", "internal", "migrate", "testdata", "web"), filepath.Join(home, "Code", "web"))
	before := treeDigest(t, home)

	o := &operatorCLI{t: t, cwd: home, env: map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, "xdg")}}
	target := filepath.Join(home, "xdg", "open-conduit", "config.json")

	// 1. Preview: valid, reported, nothing written.
	out := o.must(nil, "migrate", "config", "--from", v1, "--time-zone", "Europe/London")
	for _, want := range []string{"Validation: valid", "Preview (nothing written", "Scopes: web, api, ops", "cmd:[redacted]", "open-conduit secret set gh-app-private-key", "/schedule/web/jobs/poke/<token>", "triage-reports", "projects.api.worktree.setup", "`agent task`", "state tasks/"} {
		if !strings.Contains(out, want) {
			t.Fatalf("preview lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"ghp_not_a_real_token_literal", "op read op://vault"} {
		if strings.Contains(out, leak) {
			t.Fatalf("preview disclosed %q", leak)
		}
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("a preview wrote the config")
	}

	// 2. JSON: the same result, machine-readable.
	var r struct {
		Valid       bool              `json:"valid"`
		Diagnostics []json.RawMessage `json:"diagnostics"`
		Scopes      []string          `json:"scopes"`
		Items       []struct{ Level, Path, Message string }
		Secrets     []struct{ From, Scheme, To, Action string }
		Config      json.RawMessage `json:"config"`
	}
	o.must(&r, "migrate", "config", "--from", v1, "--time-zone", "Europe/London", "--json")
	if !r.Valid || len(r.Diagnostics) != 0 || len(r.Scopes) != 3 || len(r.Secrets) == 0 || bytes.Contains(r.Config, []byte("ghp_not")) {
		t.Fatalf("json: %+v", r)
	}

	// 3. Refusals keep conversion non-destructive.
	if _, errOut, code := o.run("migrate", "config", "--from", v1, "--out", filepath.Join(v1dir, "converted.json")); code == 0 || !strings.Contains(errOut, "Open Supervisor's config directory") {
		t.Fatalf("wrote into the v1 directory: %d %s", code, errOut)
	}
	if _, errOut, code := o.run("migrate", "config", "--from", v1, "--out", v1); code == 0 || !strings.Contains(errOut, "refusing") {
		t.Fatalf("wrote over the v1 config: %d %s", code, errOut)
	}

	// 4. Write to Open Conduit's own path, owner-only; a second write refuses.
	out = o.must(nil, "migrate", "config", "--from", v1, "--write", "--time-zone", "Europe/London")
	if !strings.Contains(out, "Written: "+target) {
		t.Fatalf("write: %s", out)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("written config: %v %v", info, err)
	}
	written, _ := os.ReadFile(target)
	if bytes.Contains(written, []byte("ghp_not")) || !bytes.Contains(written, []byte(`"cmd:op read op://vault/jira/token"`)) {
		t.Fatal("the written config must keep references (and only references)")
	}
	if got := strings.TrimSpace(o.must(nil, "config", "check", "--file", target)); got != "valid" {
		t.Fatalf("config check: %s", got)
	}
	if _, errOut, code := o.run("migrate", "config", "--from", v1, "--write"); code == 0 || !strings.Contains(errOut, "--force") {
		t.Fatalf("replaced an existing config: %d %s", code, errOut)
	}
	o.must(nil, "migrate", "config", "--from", v1, "--write", "--force", "--no-repo-files")
	if rewritten, _ := os.ReadFile(target); bytes.Contains(rewritten, []byte("from-repo")) {
		t.Fatal("--no-repo-files still read the repository file")
	}

	// 5. Every input is unchanged: the v1 config, the checkout, its repository
	//    file and playbook additions.
	if err := os.RemoveAll(filepath.Join(home, "xdg")); err != nil {
		t.Fatal(err)
	}
	if treeDigest(t, home) != before {
		t.Fatal("conversion modified an input")
	}
}
