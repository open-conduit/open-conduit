package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-conduit/client"
)

func TestJSONEditKeepsOrderAndAddressesByName(t *testing.T) {
	doc := []byte(`{"version":1,"zeta":{"b":2,"a":1},"scopes":[{"id":"web","bindings":[{"name":"agent","config":{"x":"<y>"}}],"secret_refs":{"t":"env:T"}}]}`)
	root, err := parseNode(doc)
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := pointer("/scopes/web/bindings/agent/config/model")
	if err = root.set(parts, &node{kind: 's', scalar: json.RawMessage(`"opus"`)}); err != nil {
		t.Fatal(err)
	}
	out := string(root.bytes())
	if strings.Index(out, `"zeta"`) > strings.Index(out, `"scopes"`) || strings.Index(out, `"b"`) > strings.Index(out, `"a"`) {
		t.Fatalf("key order changed:\n%s", out)
	}
	if !strings.Contains(out, `"model": "opus"`) || !strings.Contains(out, `"<y>"`) {
		t.Fatalf("edit or escaping wrong:\n%s", out)
	}
	parts, _ = pointer("/zeta/b")
	if err = root.unset(parts); err != nil || strings.Contains(string(root.bytes()), `"b"`) {
		t.Fatalf("unset: %v", err)
	}
	parts, _ = pointer("/scopes/-")
	if err = root.set(parts, &node{kind: 'o'}); err != nil || len(root.field("scopes").arr) != 2 {
		t.Fatalf("append: %v", err)
	}
	root.redact()
	if strings.Contains(string(root.bytes()), "env:T") {
		t.Fatal("reference text not redacted")
	}
	if _, err := parseNode([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestAddGitHubScope(t *testing.T) {
	ws := GitHubScope{Repo: "acme/web", Scope: "web", Connection: "github", Token: "cmd:gh auth token", Checkout: "/src/web", Holds: true}
	out, err := AddGitHubScope(nil, ws, false)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Connections []map[string]any `json:"connections"`
		Scopes      []struct {
			ID       string           `json:"id"`
			Bindings []map[string]any `json:"bindings"`
			Routes   []map[string]any `json:"routes"`
		} `json:"scopes"`
	}
	if err = json.Unmarshal(out, &cfg); err != nil || len(cfg.Connections) != 1 || len(cfg.Scopes) != 1 {
		t.Fatalf("%v %s", err, out)
	}
	s := cfg.Scopes[0]
	if len(s.Bindings) != 2 || s.Bindings[0]["resource"] == nil || len(s.Routes) != 2 || s.Routes[0]["handler"] != "github-hold" {
		t.Fatalf("scope: %+v", s)
	}
	// A second, workspace-free scope merges and keeps the first.
	free := GitHubScope{Repo: "acme/support", Scope: "support", Connection: "github", Token: "env:GH"}
	out, err = AddGitHubScope(out, free, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(out, &cfg); err != nil || len(cfg.Connections) != 1 || len(cfg.Scopes) != 2 {
		t.Fatalf("merge: %v %s", err, out)
	}
	if b := cfg.Scopes[1].Bindings[0]; b["resource"] != nil || b["requires"].(map[string]any)["workspace"] != false {
		t.Fatalf("workspace-free binding: %+v", b)
	}
	if _, err = AddGitHubScope(out, free, false); err == nil {
		t.Fatal("replaced an existing scope without --force")
	}
	if _, err = AddGitHubScope(out, free, true); err != nil {
		t.Fatal(err)
	}
}

func TestParseClaim(t *testing.T) {
	for in, want := range map[string][2]any{"12": {"", 12}, "#12": {"", 12}, "club#12": {"club", 12}, " web#3 ": {"web", 3}} {
		scope, n, ok := ParseClaim(in)
		if !ok || scope != want[0] || n != want[1] {
			t.Errorf("%q: %q %d %v", in, scope, n, ok)
		}
	}
	for _, in := range []string{"", "#", "x#", "PROJ-12", "web#-1", "Web#2"} {
		if _, _, ok := ParseClaim(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}

type setupEnv struct {
	root  string
	env   map[string]string
	stdin string
	valid func([]byte) ([]client.Detail, error)
}

func (s *setupEnv) run(args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	e := Env{Getenv: func(k string) string { return s.env[k] }, Cwd: s.root, Stdin: strings.NewReader(s.stdin), ValidateConfig: s.valid}
	code := RunOperator("test", "abc", args, e, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestInitSecretAndConfigEditing(t *testing.T) {
	root := t.TempDir()
	var validated int
	s := &setupEnv{root: root, env: map[string]string{"XDG_CONFIG_HOME": filepath.Join(root, "cfg"), "HOME": root}, valid: func(b []byte) ([]client.Detail, error) {
		validated++
		if bytes.Contains(b, []byte(`"concurrency": 0`)) {
			return []client.Detail{{Path: "/settings/concurrency", Code: "schema", Message: "must be at least 1"}}, nil
		}
		return nil, nil
	}}
	path := filepath.Join(root, "cfg", "open-conduit", "config.json")
	if _, errOut, code := s.run("init", "--token", "ghp_literal"); code != 2 || !strings.Contains(errOut, "--repo") {
		t.Fatalf("init without repo: %d %s", code, errOut)
	}
	if _, errOut, code := s.run("init", "--repo", "acme/web", "--token", "ghp_literal"); code != 2 || !strings.Contains(errOut, "reference") {
		t.Fatalf("literal token accepted: %d %s", code, errOut)
	}
	out, errOut, code := s.run("init", "--repo", "acme/web", "--no-workspace", "--token", "file:github-token", "--dry-run")
	if code != 0 || !strings.Contains(out, `"repo": "acme/web"`) {
		t.Fatalf("dry run: %d %s %s", code, out, errOut)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("dry run wrote the config")
	}
	out, errOut, code = s.run("init", "--repo", "acme/web", "--no-workspace", "--token", "file:github-token", "--holds")
	if code != 0 || !strings.Contains(out, "workspace-free") || validated == 0 {
		t.Fatalf("init: %d %s %s", code, out, errOut)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v", err)
	}
	if out, _, code = s.run("config", "path"); code != 0 || strings.TrimSpace(out) != path {
		t.Fatalf("config path: %q", out)
	}
	// Secret values come from stdin, are stored 0600 and never echoed.
	s.stdin = "tok-secret-value\n"
	out, errOut, code = s.run("secret", "set", "github-token", "--store", "file")
	if code != 0 || !strings.Contains(out, "file:github-token") || strings.Contains(out+errOut, "tok-secret-value") {
		t.Fatalf("secret set: %d %s %s", code, out, errOut)
	}
	secrets := filepath.Join(filepath.Dir(path), "secrets.json")
	if b, err := os.ReadFile(secrets); err != nil || !strings.Contains(string(b), `"github-token": "tok-secret-value"`) {
		t.Fatalf("secrets file: %s %v", b, err)
	}
	if info, _ := os.Stat(secrets); info.Mode().Perm() != 0o600 {
		t.Fatal("secrets file not owner-only")
	}
	if _, _, code = s.run("secret", "set", "github-token", "extra"); code != 2 {
		t.Fatal("secret value accepted as an argument")
	}
	// Edits are validated before saving; an invalid one leaves the file.
	before, _ := os.ReadFile(path)
	if _, errOut, code = s.run("config", "set", "/settings/concurrency", "0"); code != 1 || !strings.Contains(errOut, "/settings/concurrency") {
		t.Fatalf("invalid edit: %d %s", code, errOut)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("invalid edit changed the file")
	}
	if _, errOut, code = s.run("config", "set", "/connections/github/secret_refs/token", `"ghp_raw"`); code != 2 || !strings.Contains(errOut, "reference") {
		t.Fatalf("secret value in secret_refs accepted: %d %s", code, errOut)
	}
	if _, errOut, code = s.run("config", "set", "/connections/github/config", `{"api_url":"https://ghe.example/api/v3"}`); code != 0 {
		t.Fatalf("set: %s", errOut)
	}
	out, _, code = s.run("config", "get", "/connections/github")
	if code != 0 || !strings.Contains(out, "ghe.example") || strings.Contains(out, "file:github-token") {
		t.Fatalf("get (redacted): %s", out)
	}
	if out, _, _ = s.run("config", "show"); strings.Contains(out, "file:github-token") || !strings.Contains(out, "[redacted]") {
		t.Fatalf("show not redacted: %s", out)
	}
	if _, errOut, code = s.run("config", "unset", "/scopes/web/routes/held"); code != 0 {
		t.Fatalf("unset: %s", errOut)
	}
	if out, _, _ = s.run("config", "get", "/scopes/web/routes"); strings.Contains(out, "github-hold") {
		t.Fatalf("route not removed: %s", out)
	}
	if out, _, code = s.run("config", "check"); code != 0 || strings.TrimSpace(out) != "valid" {
		t.Fatalf("check: %d %s", code, out)
	}
}
